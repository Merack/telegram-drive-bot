package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"tg-drive-bot/internal/config"
	"tg-drive-bot/internal/repository"
)

// EmbeddingService 包含一个后台工作协程，用于异步处理新保存文件的嵌入任务
//
// 使用方法：
//  1. 使用 NewEmbeddingService 创建实例（当禁用向量搜索时返回 nil）
//  2. 调用 Start(ctx) 启动后台工作协程
//  3. 文件成功保存后调用 Enqueue(fileID) —— 该操作是非阻塞的
//  4. 在搜索过程中调用 GenerateEmbedding(text) 对查询文本进行向量化
//  5. 当 Start 传入的 ctx 被取消时，后台工作协程将停止运行;
//     RunBatch 也会观察同一个 ctx 并尽快退出

type EmbeddingService struct {
	cfg          config.EmbeddingConfig
	client       *http.Client
	queue        chan uint // buffered channel of file IDs to process (realtime worker)
	repo         FileRepoForEmbedding
	batchRunning atomic.Bool // CAS-protected single-instance lock for RunBatch
	// lifeCtx 在 Start 时记录,供 RunBatch 复用 -- 进程级 SIGTERM 也能 cancel 长跑批量任务
	// nil 时(Start 未调用)RunBatch 退化为 context.Background()
	lifeCtx atomic.Pointer[context.Context]
}

// FileRepoForEmbedding 是 EmbeddingService 需要的 FileRepository 子集
// 用接口形式来源是为了避免 service 包内部循环引用 repository
// (实现端在 *repository.FileRepo 上)
type FileRepoForEmbedding interface {
	ListIDsMissingEmbedding() ([]uint, error)
	ListAllFileIDs() ([]uint, error)
	GetEmbeddingSource(fileID uint) (*repository.EmbeddingSource, error)
	SaveEmbedding(fileID uint, vecJSON string) error
}

// BatchMode 控制批量任务覆盖的范围
type BatchMode string

const (
	// BatchModeRefill 只处理 embedding 列为 NULL 的文件(补缺失)
	BatchModeRefill BatchMode = "refill"
	// BatchModeRedo 处理全部文件(覆盖式重做)
	BatchModeRedo BatchMode = "redo"
)

// BatchProgress 表示批量任务的阶段性进度,通过 chan 暴露给 handler 层
type BatchProgress struct {
	Done  int
	Total int
	Phase string // "scanning" | "running" | "done" | "failed"
	Err   error  // 仅 Phase=="failed" 时非 nil
}

// ErrBatchBusy 由 RunBatch 在已有批量任务运行时返回
var ErrBatchBusy = errors.New("embedding batch already running")

// openaiEmbeddingRequest mirrors the OpenAI embeddings API request body.
type openaiEmbeddingRequest struct {
	Input      string `json:"input"`
	Model      string `json:"model"`
	Dimensions int    `json:"dimensions,omitempty"`
}

// openaiEmbeddingResponse mirrors the OpenAI embeddings API response.
type openaiEmbeddingResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// geminiEmbedRequest mirrors the Gemini embedContent API request body.
type geminiEmbedRequest struct {
	Model                string        `json:"model"`
	Content              geminiContent `json:"content"`
	OutputDimensionality int           `json:"outputDimensionality,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

// geminiEmbedResponse mirrors the Gemini embedContent API response.
type geminiEmbedResponse struct {
	Embedding *struct {
		Values []float64 `json:"values"`
	} `json:"embedding"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

const (
	embeddingQueueSize    = 5000
	embeddingHTTPTimeout  = 30 * time.Second
	embeddingRetryDelay   = 5 * time.Second
	embeddingMaxRetries   = 3
	embeddingWorkerPeriod = 100 * time.Millisecond // drain delay between jobs
)

// NewEmbeddingService creates an EmbeddingService. Returns nil when vector search is disabled,
// so callers can safely check for nil before using it.
func NewEmbeddingService(cfg config.EmbeddingConfig, repo FileRepoForEmbedding) *EmbeddingService {
	if !cfg.Enabled {
		return nil
	}
	return &EmbeddingService{
		cfg:    cfg,
		client: &http.Client{Timeout: embeddingHTTPTimeout},
		queue:  make(chan uint, embeddingQueueSize),
		repo:   repo,
	}
}

// Enqueue adds a file ID to the embedding generation queue. Non-blocking; if the queue is
// full, the file ID is dropped with a warning (the embedding can be regenerated later).
func (s *EmbeddingService) Enqueue(fileID uint) {
	select {
	case s.queue <- fileID:
	default:
		slog.Warn("embedding queue full, dropping file", "file_db_id", fileID)
	}
}

// Threshold returns the configured maximum cosine distance for vector search.
func (s *EmbeddingService) Threshold() float64 {
	return s.cfg.Threshold
}

// Start launches the background worker that consumes the queue and generates embeddings.
// It blocks until ctx is cancelled, so call it in a goroutine.
// 同时把 ctx 存到 lifeCtx 里, RunBatch 会复用 -- 这样进程 SIGTERM 时,
// 正在跑的 /emb_re 批量也能跟着停, 而不是无视 cancel 一直跑到底
func (s *EmbeddingService) Start(ctx context.Context) {
	s.lifeCtx.Store(&ctx)
	slog.Info("embedding worker started",
		"api_type", s.cfg.APIType,
		"model", s.cfg.Model,
		"dimensions", s.cfg.Dimensions,
		"base_url", s.cfg.BaseURL)

	for {
		select {
		case <-ctx.Done():
			slog.Info("embedding worker stopped")
			return
		case fileID := <-s.queue:
			s.processFile(ctx, fileID)
			// Small delay to avoid hammering the API when many files are enqueued at once.
			if !sleepOrDone(ctx, embeddingWorkerPeriod) {
				slog.Info("embedding worker stopped")
				return
			}
		}
	}
}

// GenerateEmbedding calls the embedding API to generate a vector for the given text.
// Used during search to embed the user's query.
func (s *EmbeddingService) GenerateEmbedding(ctx context.Context, text string) ([]float64, error) {
	return s.callEmbeddingAPI(ctx, text)
}

// 阶段性进度的节流参数:每处理 N 个发一次 / 距上次发送超过 T 秒发一次
// 避免 handler 端对同一条 Telegram 消息高频 editMessageText 触发 429
const (
	batchProgressEvery    = 50
	batchProgressInterval = 10 * time.Second
)

// RunBatch 用独立 goroutine 跑全量 / 补缺失的 embedding 任务
// 调用方应在自己的 goroutine 里运行,并消费 progress chan(本方法负责 close)
// 同一时刻只能有一个 RunBatch 在跑;重复调用立即返回 ErrBatchBusy
//
// 任务使用 Start 时传入的进程级 lifeCtx, 这样 SIGTERM 也能 cancel 长跑任务,
// 不再像旧版本那样要求调用方自行管理一个不被 handler return 影响的 ctx
// 若 Start 未被调用(测试场景), 退化为 context.Background()
func (s *EmbeddingService) RunBatch(mode BatchMode, progress chan<- BatchProgress) error {
	if !s.batchRunning.CompareAndSwap(false, true) {
		close(progress)
		return ErrBatchBusy
	}
	defer s.batchRunning.Store(false)
	defer close(progress)

	ctx := context.Background()
	if p := s.lifeCtx.Load(); p != nil {
		ctx = *p
	}

	if s.repo == nil {
		err := fmt.Errorf("embedding batch unavailable: nil repo")
		progress <- BatchProgress{Phase: "failed", Err: err}
		return err
	}

	progress <- BatchProgress{Phase: "scanning"}

	var (
		ids []uint
		err error
	)
	switch mode {
	case BatchModeRefill:
		ids, err = s.repo.ListIDsMissingEmbedding()
	case BatchModeRedo:
		ids, err = s.repo.ListAllFileIDs()
	default:
		err = fmt.Errorf("unknown batch mode: %s", mode)
	}
	if err != nil {
		progress <- BatchProgress{Phase: "failed", Err: err}
		return err
	}

	total := len(ids)
	slog.Info("embedding batch started", "mode", mode, "total", total)
	progress <- BatchProgress{Done: 0, Total: total, Phase: "running"}

	if total == 0 {
		progress <- BatchProgress{Done: 0, Total: 0, Phase: "done"}
		return nil
	}

	lastSent := time.Now()
	for i, id := range ids {
		if ctx.Err() != nil {
			err := ctx.Err()
			slog.Warn("embedding batch cancelled", "done", i, "total", total, "error", err)
			progress <- BatchProgress{Done: i, Total: total, Phase: "failed", Err: err}
			return err
		}
		s.processFile(ctx, id)
		// 节流(沿用实时 worker 的间隔,避免对 API 形成爆发流量)
		if !sleepOrDone(ctx, embeddingWorkerPeriod) {
			err := ctx.Err()
			slog.Warn("embedding batch cancelled mid-sleep", "done", i + 1, "total", total)
			progress <- BatchProgress{Done: i + 1, Total: total, Phase: "failed", Err: err}
			return err
		}

		done := i + 1
		if done == total || done%batchProgressEvery == 0 || time.Since(lastSent) >= batchProgressInterval {
			// 非阻塞投递:消费者落后时直接丢弃,避免 RunBatch 卡死
			select {
			case progress <- BatchProgress{Done: done, Total: total, Phase: "running"}:
				lastSent = time.Now()
			default:
			}
		}
	}

	progress <- BatchProgress{Done: total, Total: total, Phase: "done"}
	slog.Info("embedding batch completed", "mode", mode, "total", total)
	return nil
}

// processFile reads a file's text content from DB, generates an embedding, and writes it back.
func (s *EmbeddingService) processFile(ctx context.Context, fileID uint) {
	src, err := s.repo.GetEmbeddingSource(fileID)
	if err != nil {
		slog.Error("embedding: failed to read file", "file_db_id", fileID, "error", err)
		return
	}

	text := buildEmbeddingInput(src.FileName, src.Title, src.Caption)
	if text == "" {
		slog.Debug("embedding: skipping file with no text content", "file_db_id", fileID)
		return
	}

	var embedding []float64
	for attempt := 1; attempt <= embeddingMaxRetries; attempt++ {
		embedding, err = s.callEmbeddingAPI(ctx, text)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return // context cancelled, stop retrying
		}
		slog.Warn("embedding: API call failed, retrying",
			"file_db_id", fileID, "attempt", attempt, "error", err)
		if !sleepOrDone(ctx, embeddingRetryDelay) {
			return // context cancelled during backoff
		}
	}
	if err != nil {
		slog.Error("embedding: giving up after retries", "file_db_id", fileID, "error", err)
		return
	}

	// Write the embedding back via the repository (avoids holding a *gorm.DB in the service layer).
	embJSON, err := json.Marshal(embedding)
	if err != nil {
		slog.Error("embedding: failed to marshal vector", "file_db_id", fileID, "error", err)
		return
	}
	if err := s.repo.SaveEmbedding(fileID, string(embJSON)); err != nil {
		slog.Error("embedding: failed to save vector", "file_db_id", fileID, "error", err)
		return
	}

	slog.Info("embedding: generated", "file_db_id", fileID, "dimensions", len(embedding))
}

// sleepOrDone 等待 d 时长, 或在 ctx 被取消时立即返回
// 返回 true 表示正常睡满, false 表示被 ctx cancel 提前唤醒
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// callEmbeddingAPI dispatches to the correct API implementation based on configured APIType.
func (s *EmbeddingService) callEmbeddingAPI(ctx context.Context, text string) ([]float64, error) {
	switch s.cfg.APIType {
	case "gemini":
		return s.callGeminiAPI(ctx, text)
	default:
		return s.callOpenAIAPI(ctx, text)
	}
}

// callOpenAIAPI sends a request to an OpenAI-compatible embedding endpoint.
func (s *EmbeddingService) callOpenAIAPI(ctx context.Context, text string) ([]float64, error) {
	reqBody := openaiEmbeddingRequest{
		Input:      text,
		Model:      s.cfg.Model,
		Dimensions: s.cfg.Dimensions,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(s.cfg.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)

	respBytes, err := s.doRequest(req)
	if err != nil {
		return nil, err
	}

	var embResp openaiEmbeddingResponse
	if err := json.Unmarshal(respBytes, &embResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if embResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", embResp.Error.Message)
	}
	if len(embResp.Data) == 0 || len(embResp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("API returned empty embedding")
	}

	return embResp.Data[0].Embedding, nil
}

// callGeminiAPI sends a request to Google's Gemini embedContent endpoint.
// URL: {baseURL}/models/{model}:embedContent?key={apiKey}
func (s *EmbeddingService) callGeminiAPI(ctx context.Context, text string) ([]float64, error) {
	reqBody := geminiEmbedRequest{
		Model: "models/" + s.cfg.Model,
		Content: geminiContent{
			Parts: []geminiPart{{Text: text}},
		},
		OutputDimensionality: s.cfg.Dimensions,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/models/%s:embedContent?key=%s",
		strings.TrimRight(s.cfg.BaseURL, "/"), s.cfg.Model, s.cfg.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	respBytes, err := s.doRequest(req)
	if err != nil {
		return nil, err
	}

	var embResp geminiEmbedResponse
	if err := json.Unmarshal(respBytes, &embResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if embResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", embResp.Error.Message)
	}
	if embResp.Embedding == nil || len(embResp.Embedding.Values) == 0 {
		return nil, fmt.Errorf("API returned empty embedding")
	}

	return embResp.Embedding.Values, nil
}

// doRequest executes an HTTP request and returns the response body bytes.
func (s *EmbeddingService) doRequest(req *http.Request) ([]byte, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBytes))
	}

	return respBytes, nil
}

// 将多个文本字段拼接成一个字符串以用于生成嵌入向量
// 如果没有任何有效的文本内容，则返回空字符串
func buildEmbeddingInput(fileName, title, caption string) string {
	var parts []string
	if fileName != "" {
		parts = append(parts, fileName)
	}
	if title != "" {
		parts = append(parts, title)
	}
	if caption != "" {
		parts = append(parts, caption)
	}
	return strings.Join(parts, " | ")
}

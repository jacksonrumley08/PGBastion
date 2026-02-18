package cluster

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	pgRewindProgress = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pgbastion_pg_rewind_progress_percent",
		Help: "Current pg_rewind progress percentage (0-100)",
	})

	pgRewindBytesTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pgbastion_pg_rewind_bytes_total",
		Help: "Total bytes to copy during pg_rewind",
	})

	pgRewindBytesCopied = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pgbastion_pg_rewind_bytes_copied",
		Help: "Bytes copied so far during pg_rewind",
	})

	pgRewindDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "pgbastion_pg_rewind_duration_seconds",
		Help:    "Duration of pg_rewind operations",
		Buckets: prometheus.ExponentialBuckets(10, 2, 10), // 10s to ~2.5h
	})

	pgRewindTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_pg_rewind_total",
		Help: "Total number of pg_rewind operations",
	}, []string{"result"})
)

// RewindPhase represents the current phase of pg_rewind.
type RewindPhase string

const (
	RewindPhaseIdle       RewindPhase = "idle"
	RewindPhaseConnecting RewindPhase = "connecting"
	RewindPhaseFetching   RewindPhase = "fetching_files"
	RewindPhaseCopying    RewindPhase = "copying"
	RewindPhaseComplete   RewindPhase = "complete"
	RewindPhaseFailed     RewindPhase = "failed"
)

// RewindProgress holds the current progress of a pg_rewind operation.
type RewindProgress struct {
	Phase         RewindPhase `json:"phase"`
	Percent       float64     `json:"percent"`
	BytesTotal    int64       `json:"bytes_total"`
	BytesCopied   int64       `json:"bytes_copied"`
	FilesTotal    int         `json:"files_total"`
	FilesCopied   int         `json:"files_copied"`
	StartTime     time.Time   `json:"start_time"`
	LastUpdate    time.Time   `json:"last_update"`
	Error         string      `json:"error,omitempty"`
	Output        []string    `json:"output,omitempty"` // Recent output lines
}

// ProgressTracker tracks progress for long-running operations like pg_rewind.
type ProgressTracker struct {
	mu       sync.RWMutex
	rewind   RewindProgress
	logger   *slog.Logger
}

// NewProgressTracker creates a new ProgressTracker.
func NewProgressTracker(logger *slog.Logger) *ProgressTracker {
	return &ProgressTracker{
		rewind: RewindProgress{Phase: RewindPhaseIdle},
		logger: logger.With("component", "progress-tracker"),
	}
}

// GetRewindProgress returns the current pg_rewind progress.
func (p *ProgressTracker) GetRewindProgress() RewindProgress {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.rewind
}

// ResetRewind resets the rewind progress to idle.
func (p *ProgressTracker) ResetRewind() {
	p.mu.Lock()
	p.rewind = RewindProgress{Phase: RewindPhaseIdle}
	p.mu.Unlock()
	pgRewindProgress.Set(0)
	pgRewindBytesTotal.Set(0)
	pgRewindBytesCopied.Set(0)
}

// Patterns for parsing pg_rewind output.
var (
	// "source and target cluster are on the same timeline"
	sameTimelinePattern = regexp.MustCompile(`same timeline`)

	// "rewinding from 0/3000060 to 0/4000028"
	rewindingPattern = regexp.MustCompile(`rewinding from (\S+) to (\S+)`)

	// "Done!"
	donePattern = regexp.MustCompile(`^Done!`)

	// "need to copy 26 MB"
	copyPattern = regexp.MustCompile(`need to copy (\d+(?:\.\d+)?)\s*(MB|GB|KB|bytes)`)

	// Progress indicators (varies by PostgreSQL version)
	// Some versions: "26/26 kB (100%) copied"
	// Others: "copying \"base/12345/16384\"" with progress
	progressPattern = regexp.MustCompile(`(\d+)/(\d+)\s*(kB|MB|GB|bytes)\s*\((\d+)%\)`)

	// File copying: "copying \"path/to/file\""
	filePattern = regexp.MustCompile(`copying "([^"]+)"`)
)

// RunPgRewindWithProgress runs pg_rewind and tracks progress.
func (p *ProgressTracker) RunPgRewindWithProgress(ctx context.Context, pgRewindPath, dataDir, sourceServer string) error {
	p.mu.Lock()
	p.rewind = RewindProgress{
		Phase:     RewindPhaseConnecting,
		StartTime: time.Now(),
		LastUpdate: time.Now(),
	}
	p.mu.Unlock()

	cmd := exec.CommandContext(ctx, pgRewindPath,
		"--target-pgdata="+dataDir,
		"--source-server="+sourceServer,
		"--progress",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		p.setRewindError(fmt.Sprintf("failed to create stdout pipe: %v", err))
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		p.setRewindError(fmt.Sprintf("failed to create stderr pipe: %v", err))
		return err
	}

	if err := cmd.Start(); err != nil {
		p.setRewindError(fmt.Sprintf("failed to start pg_rewind: %v", err))
		pgRewindTotal.WithLabelValues("error").Inc()
		return err
	}

	// Create a combined reader from stdout and stderr.
	go p.parseOutput(io.MultiReader(stdout, stderr))

	err = cmd.Wait()
	duration := time.Since(p.rewind.StartTime)
	pgRewindDuration.Observe(duration.Seconds())

	if err != nil {
		p.setRewindError(fmt.Sprintf("pg_rewind failed: %v", err))
		pgRewindTotal.WithLabelValues("error").Inc()
		return err
	}

	p.mu.Lock()
	p.rewind.Phase = RewindPhaseComplete
	p.rewind.Percent = 100
	p.rewind.LastUpdate = time.Now()
	p.mu.Unlock()

	pgRewindProgress.Set(100)
	pgRewindTotal.WithLabelValues("success").Inc()
	p.logger.Info("pg_rewind completed", "duration", duration)

	return nil
}

func (p *ProgressTracker) setRewindError(errMsg string) {
	p.mu.Lock()
	p.rewind.Phase = RewindPhaseFailed
	p.rewind.Error = errMsg
	p.rewind.LastUpdate = time.Now()
	p.mu.Unlock()
	p.logger.Error("pg_rewind failed", "error", errMsg)
}

func (p *ProgressTracker) parseOutput(r io.Reader) {
	scanner := bufio.NewScanner(r)
	outputBuffer := make([]string, 0, 50)

	for scanner.Scan() {
		line := scanner.Text()

		// Keep last 50 lines.
		if len(outputBuffer) >= 50 {
			outputBuffer = outputBuffer[1:]
		}
		outputBuffer = append(outputBuffer, line)

		p.mu.Lock()
		p.rewind.Output = make([]string, len(outputBuffer))
		copy(p.rewind.Output, outputBuffer)
		p.rewind.LastUpdate = time.Now()

		// Parse progress from the line.
		if matches := progressPattern.FindStringSubmatch(line); len(matches) >= 5 {
			if percent, err := strconv.ParseFloat(matches[4], 64); err == nil {
				p.rewind.Percent = percent
				p.rewind.Phase = RewindPhaseCopying
				pgRewindProgress.Set(percent)
			}
			if copied, err := strconv.ParseInt(matches[1], 10, 64); err == nil {
				p.rewind.BytesCopied = p.parseBytes(copied, matches[3])
				pgRewindBytesCopied.Set(float64(p.rewind.BytesCopied))
			}
			if total, err := strconv.ParseInt(matches[2], 10, 64); err == nil {
				p.rewind.BytesTotal = p.parseBytes(total, matches[3])
				pgRewindBytesTotal.Set(float64(p.rewind.BytesTotal))
			}
		} else if matches := copyPattern.FindStringSubmatch(line); len(matches) >= 3 {
			if size, err := strconv.ParseFloat(matches[1], 64); err == nil {
				p.rewind.BytesTotal = p.parseBytes(int64(size), matches[2])
				pgRewindBytesTotal.Set(float64(p.rewind.BytesTotal))
			}
			p.rewind.Phase = RewindPhaseFetching
		} else if rewindingPattern.MatchString(line) {
			p.rewind.Phase = RewindPhaseFetching
		} else if filePattern.MatchString(line) {
			p.rewind.Phase = RewindPhaseCopying
			p.rewind.FilesCopied++
		} else if donePattern.MatchString(line) {
			p.rewind.Phase = RewindPhaseComplete
			p.rewind.Percent = 100
		}

		p.mu.Unlock()
	}
}

func (p *ProgressTracker) parseBytes(value int64, unit string) int64 {
	switch unit {
	case "GB":
		return value * 1024 * 1024 * 1024
	case "MB":
		return value * 1024 * 1024
	case "kB", "KB":
		return value * 1024
	default:
		return value
	}
}

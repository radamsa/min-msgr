package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/realtime"
)

// RetentionPublisher broadcasts retention-driven deletions to web clients.
type RetentionPublisher interface {
	Publish(chatID int64, event realtime.Event)
}

// RetentionReport describes what a single retention run removed.
type RetentionReport struct {
	MessagesDeleted int64
	ChatsTrimmed    int
	SessionsDeleted int64
	ArchivesDeleted int64
	ArchivesFailed  int64
}

// RetentionService enforces time-based storage limits. Every operation is
// idempotent, so the service can be restarted or run concurrently without
// losing or double-deleting data.
type RetentionService struct {
	pool        *pgxpool.Pool
	archivesDir string
	publisher   RetentionPublisher
}

// NewRetentionService returns the retention cron. The publisher may be nil.
func NewRetentionService(pool *pgxpool.Pool, archivesDir string, publisher RetentionPublisher) *RetentionService {
	if abs, err := filepath.Abs(archivesDir); err == nil {
		archivesDir = abs
	}
	return &RetentionService{pool: pool, archivesDir: archivesDir, publisher: publisher}
}

// RunOnce deletes messages older than each chat's days limit and archives past
// their expiry. Archive files are removed before their database rows, so a
// failed file deletion is retried on the next run.
func (s *RetentionService) RunOnce(ctx context.Context) (RetentionReport, error) {
	var report RetentionReport

	trims, err := s.deleteExpiredMessages(ctx)
	if err != nil {
		return report, err
	}
	for _, trim := range trims {
		report.MessagesDeleted += trim.Deleted
	}
	report.ChatsTrimmed = len(trims)
	s.publishTrims(trims)

	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	if err != nil {
		return report, fmt.Errorf("delete expired sessions: %w", err)
	}
	report.SessionsDeleted = tag.RowsAffected()

	archives, err := s.expiredArchives(ctx)
	if err != nil {
		return report, err
	}
	for _, archive := range archives {
		if err := s.removeArchiveFile(archive.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			report.ArchivesFailed++
			slog.Error("retention: delete archive file",
				"archive_id", archive.id, "path", archive.path, "error", err)
			continue
		}
		tag, err := s.pool.Exec(ctx, `DELETE FROM archives WHERE id = $1`, archive.id)
		if err != nil {
			return report, fmt.Errorf("delete archive row %d: %w", archive.id, err)
		}
		report.ArchivesDeleted += tag.RowsAffected()
	}
	return report, nil
}

// deleteExpiredMessages removes messages past their chat's days limit and
// returns per-chat trim results so web clients can be notified.
func (s *RetentionService) deleteExpiredMessages(ctx context.Context) ([]domain.TrimResult, error) {
	rows, err := s.pool.Query(ctx, `
		DELETE FROM messages m
		USING chats c
		WHERE m.chat_id = c.id
			AND m.created_at < now() - make_interval(days => c.days_limit)
		RETURNING m.chat_id, m.seq
	`)
	if err != nil {
		return nil, fmt.Errorf("delete expired messages: %w", err)
	}
	defer rows.Close()

	results := make(map[int64]*domain.TrimResult)
	if err := collectTrimmed(rows, results); err != nil {
		return nil, err
	}
	ordered := make([]domain.TrimResult, 0, len(results))
	for _, result := range results {
		ordered = append(ordered, *result)
	}
	return ordered, nil
}

func (s *RetentionService) publishTrims(trims []domain.TrimResult) {
	if s.publisher == nil {
		return
	}
	for _, trim := range trims {
		s.publisher.Publish(trim.ChatID, realtime.Event{
			Type: "history.trimmed",
			Payload: realtime.HistoryTrimmed{
				ChatID:     trim.ChatID,
				Deleted:    trim.Deleted,
				ThroughSeq: trim.ThroughSeq,
			},
		})
	}
}

type expiredArchive struct {
	id   int64
	path string
}

func (s *RetentionService) expiredArchives(ctx context.Context) ([]expiredArchive, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, file_path FROM archives WHERE expires_at <= now() ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("select expired archives: %w", err)
	}
	defer rows.Close()

	var archives []expiredArchive
	for rows.Next() {
		var archive expiredArchive
		if err := rows.Scan(&archive.id, &archive.path); err != nil {
			return nil, fmt.Errorf("scan expired archive: %w", err)
		}
		archives = append(archives, archive)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired archives: %w", err)
	}
	return archives, nil
}

// removeArchiveFile deletes an archive file only if it lives under the
// configured archives directory. This keeps a corrupt or malicious file_path
// row from deleting unrelated files.
func (s *RetentionService) removeArchiveFile(path string) error {
	target, err := resolveArchivePath(s.archivesDir, path)
	if err != nil {
		return err
	}
	return os.Remove(target)
}

// Run executes RunOnce immediately and then on every tick until ctx is done.
// Errors are logged and do not stop the loop; the next tick retries.
func (s *RetentionService) Run(ctx context.Context, interval time.Duration) {
	s.runAndLog(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runAndLog(ctx)
		}
	}
}

func (s *RetentionService) runAndLog(ctx context.Context) {
	started := time.Now()
	report, err := s.RunOnce(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; a partial run will be retried next start
		}
		slog.Error("retention run failed", "error", err)
		return
	}
	if report.MessagesDeleted == 0 && report.SessionsDeleted == 0 &&
		report.ArchivesDeleted == 0 && report.ArchivesFailed == 0 {
		slog.Debug("retention run completed", "duration", time.Since(started))
		return
	}
	slog.Info("retention run completed",
		"messages_deleted", report.MessagesDeleted,
		"chats_trimmed", report.ChatsTrimmed,
		"sessions_deleted", report.SessionsDeleted,
		"archives_deleted", report.ArchivesDeleted,
		"archives_failed", report.ArchivesFailed,
		"duration", time.Since(started),
	)
}

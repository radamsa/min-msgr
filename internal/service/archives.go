package service

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/domain"
)

// ArchiveRetention is how long a revoked chat's archive stays downloadable.
const ArchiveRetention = 30 * 24 * time.Hour

// Archive is a revoked chat's downloadable zip.
type Archive struct {
	ID        int64
	ChatID    int64
	Label     string
	FilePath  string
	SizeBytes int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ArchiveService creates archives on revocation and serves them afterwards.
type ArchiveService struct {
	pool *pgxpool.Pool
	dir  string
}

func NewArchiveService(pool *pgxpool.Pool, archivesDir string) *ArchiveService {
	if abs, err := filepath.Abs(archivesDir); err == nil {
		archivesDir = abs
	}
	return &ArchiveService{pool: pool, dir: archivesDir}
}

// CreateAndDeleteChat snapshots the chat into a zip, records the archive, and
// deletes the chat with all its messages and the API key in one transaction.
// If any step fails, the temporary and final archive files are removed.
func (s *ArchiveService) CreateAndDeleteChat(
	ctx context.Context,
	userID, chatID int64,
) (Archive, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Archive{}, fmt.Errorf("begin archive chat: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	var label string
	var chatCreatedAt time.Time
	var publicID string
	err = tx.QueryRow(ctx, `
		SELECT c.label, c.created_at, u.public_id
		FROM chats c
		JOIN users u ON u.id = c.user_id
		WHERE c.id = $1 AND c.user_id = $2
		FOR UPDATE OF c
	`, chatID, userID).Scan(&label, &chatCreatedAt, &publicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Archive{}, domain.ErrNotFound
	}
	if err != nil {
		return Archive{}, fmt.Errorf("lock chat for archive: %w", err)
	}

	var messageCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE chat_id = $1`, chatID).
		Scan(&messageCount); err != nil {
		return Archive{}, fmt.Errorf("count archived messages: %w", err)
	}

	now := time.Now().UTC()
	base := archiveBaseName(label, chatID, now)
	finalPath := filepath.Join(s.dir, base+".zip")

	tmp, err := os.CreateTemp(s.dir, ".archive-*.zip")
	if err != nil {
		return Archive{}, fmt.Errorf("create archive file: %w", err)
	}
	tmpPath := tmp.Name()
	keepFile := false
	defer func() {
		_ = tmp.Close()
		if !keepFile {
			_ = os.Remove(tmpPath)
			_ = os.Remove(finalPath)
		}
	}()

	zipWriter := zip.NewWriter(tmp)
	entry, err := zipWriter.Create(base + ".md")
	if err != nil {
		return Archive{}, fmt.Errorf("create archive entry: %w", err)
	}
	if err := writeArchiveMarkdown(
		ctx, entry, tx, label, publicID, chatID, chatCreatedAt, now, messageCount,
	); err != nil {
		return Archive{}, err
	}
	if err := zipWriter.Close(); err != nil {
		return Archive{}, fmt.Errorf("finish archive zip: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Archive{}, fmt.Errorf("close archive file: %w", err)
	}

	info, err := os.Stat(tmpPath)
	if err != nil {
		return Archive{}, fmt.Errorf("stat archive file: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return Archive{}, fmt.Errorf("move archive file: %w", err)
	}

	var archive Archive
	err = tx.QueryRow(ctx, `
		INSERT INTO archives (user_id, chat_id, label, file_path, size_bytes, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, chat_id, label, file_path, size_bytes, created_at, expires_at
	`, userID, chatID, label, finalPath, info.Size(), now, now.Add(ArchiveRetention)).Scan(
		&archive.ID, &archive.ChatID, &archive.Label, &archive.FilePath,
		&archive.SizeBytes, &archive.CreatedAt, &archive.ExpiresAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return Archive{}, domain.ErrConflict
		}
		return Archive{}, fmt.Errorf("insert archive: %w", err)
	}

	tag, err := tx.Exec(ctx, `DELETE FROM chats WHERE id = $1 AND user_id = $2`, chatID, userID)
	if err != nil {
		return Archive{}, fmt.Errorf("delete archived chat: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return Archive{}, domain.ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return Archive{}, fmt.Errorf("commit archive chat: %w", err)
	}

	keepFile = true
	return archive, nil
}

// List returns the user's non-expired archives, soonest expiry first.
func (s *ArchiveService) List(ctx context.Context, userID int64) ([]Archive, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, chat_id, label, file_path, size_bytes, created_at, expires_at
		FROM archives
		WHERE user_id = $1 AND expires_at > now()
		ORDER BY expires_at ASC, id ASC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list archives: %w", err)
	}
	defer rows.Close()

	archives := make([]Archive, 0)
	for rows.Next() {
		var archive Archive
		if err := rows.Scan(
			&archive.ID, &archive.ChatID, &archive.Label, &archive.FilePath,
			&archive.SizeBytes, &archive.CreatedAt, &archive.ExpiresAt,
		); err != nil {
			return nil, fmt.Errorf("scan archive: %w", err)
		}
		archives = append(archives, archive)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate archives: %w", err)
	}
	return archives, nil
}

// Open returns archive metadata and an open file for download. The path is
// checked against the configured archives directory.
func (s *ArchiveService) Open(ctx context.Context, userID, archiveID int64) (Archive, *os.File, error) {
	var archive Archive
	err := s.pool.QueryRow(ctx, `
		SELECT id, chat_id, label, file_path, size_bytes, created_at, expires_at
		FROM archives
		WHERE id = $1 AND user_id = $2 AND expires_at > now()
	`, archiveID, userID).Scan(
		&archive.ID, &archive.ChatID, &archive.Label, &archive.FilePath,
		&archive.SizeBytes, &archive.CreatedAt, &archive.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Archive{}, nil, domain.ErrNotFound
	}
	if err != nil {
		return Archive{}, nil, fmt.Errorf("find archive: %w", err)
	}

	path, err := resolveArchivePath(s.dir, archive.FilePath)
	if err != nil {
		return Archive{}, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Archive{}, nil, fmt.Errorf("open archive file: %w", err)
	}
	return archive, file, nil
}

func writeArchiveMarkdown(
	ctx context.Context,
	w io.Writer,
	tx pgx.Tx,
	label, publicID string,
	chatID int64,
	chatCreatedAt, exportedAt time.Time,
	messageCount int64,
) error {
	if _, err := fmt.Fprintf(w, "# %s\n\n", label); err != nil {
		return fmt.Errorf("write archive header: %w", err)
	}
	if _, err := fmt.Fprintf(w,
		"- Пользователь: %s\n- Чат: %d\n- Создан: %s\n- Сообщений: %d\n- Архив создан: %s\n\n",
		publicID, chatID, chatCreatedAt.Format(time.RFC3339), messageCount,
		exportedAt.Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("write archive metadata: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT m.created_at,
			CASE WHEN m.author_type = 'agent'
				THEN m.agent_name || '@' || u.oauth_login
				ELSE u.oauth_login END,
			m.body
		FROM messages m
		JOIN chats c ON c.id = m.chat_id
		JOIN users u ON u.id = c.user_id
		WHERE m.chat_id = $1
		ORDER BY m.seq ASC
	`, chatID)
	if err != nil {
		return fmt.Errorf("read archived messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var createdAt time.Time
		var author, body string
		if err := rows.Scan(&createdAt, &author, &body); err != nil {
			return fmt.Errorf("scan archived message: %w", err)
		}
		if _, err := fmt.Fprintf(w, "## %s %s\n\n%s\n\n",
			createdAt.Format(time.RFC3339), author, body); err != nil {
			return fmt.Errorf("write archived message: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate archived messages: %w", err)
	}
	return nil
}

func archiveBaseName(label string, chatID int64, at time.Time) string {
	safe := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) ||
			r == '-' || r == '_' || r == '.' || r == ' ' {
			return r
		}
		return '_'
	}, label)
	safe = strings.TrimSpace(safe)
	if safe == "" {
		safe = "chat"
	}
	runes := []rune(safe)
	if len(runes) > 80 {
		safe = string(runes[:80])
	}
	return fmt.Sprintf("%s_%d_%s", safe, chatID, at.Format("2006-01-02"))
}

func resolveArchivePath(dir, path string) (string, error) {
	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return "", fmt.Errorf("resolve archive path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive path %q is outside %q", target, dir)
	}
	return target, nil
}

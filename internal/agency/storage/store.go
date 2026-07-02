package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agency-two/internal/agency/provider"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := preparePrivateStateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := preparePrivateDBFile(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if err := s.applyPragmas(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.assertWALMode(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := chmodPrivateDBFiles(path); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func preparePrivateStateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%s permissions %s are wider than 0700", path, info.Mode().Perm())
	}
	return os.Chmod(path, 0700)
}

func preparePrivateDBFile(path string) error {
	if err := assertPrivateFile(path); err != nil {
		return err
	}
	if err := assertPrivateFile(path + "-wal"); err != nil {
		return err
	}
	if err := assertPrivateFile(path + "-shm"); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	return file.Close()
}

func assertPrivateFile(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%s permissions %s are wider than 0600", path, info.Mode().Perm())
	}
	return nil
}

func chmodPrivateDBFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0600); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.DB.Close()
}

func (s *Store) Export(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("export path is required")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("export path already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `VACUUM INTO `+sqlString(path)); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func (s *Store) applyPragmas(ctx context.Context) error {
	pragmas := []string{
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_size_limit=67108864",
	}
	for _, p := range pragmas {
		if _, err := s.DB.ExecContext(ctx, p); err != nil {
			return err
		}
	}
	var enabled int
	if err := s.DB.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
		return err
	}
	if enabled != 1 {
		return errors.New("foreign_keys pragma did not enable")
	}
	return nil
}

func (s *Store) assertWALMode(ctx context.Context) error {
	var journalMode string
	if err := s.DB.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return err
	}
	if strings.ToLower(journalMode) != "wal" {
		return fmt.Errorf("journal_mode=%s, want wal", journalMode)
	}
	return nil
}

func sqlString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (s *Store) Migrate(ctx context.Context) error {
	var table string
	err := s.DB.QueryRowContext(ctx, "select name from sqlite_master where type='table' and name='schema_migrations'").Scan(&table)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		rows, err := s.DB.QueryContext(ctx, "select version from schema_migrations")
		if err != nil {
			return err
		}
		defer rows.Close()
		seen := false
		for rows.Next() {
			var version string
			if err := rows.Scan(&version); err != nil {
				return err
			}
			if version != "1" {
				return fmt.Errorf("unknown schema version %s", version)
			}
			seen = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if seen {
			return nil
		}
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, stmt := range schemaStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration 1 failed: %w\n%s", err, stmt)
		}
	}
	if _, err := tx.ExecContext(ctx, "insert into schema_migrations(version, applied_at) values(?, ?)", "1", Now()); err != nil {
		tx.Rollback()
		return err
	}
	if err := seedCatalog(ctx, tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		return err
	}
	return nil
}

func Now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	now := uint64(time.Now().UnixMilli())
	b[0] = byte(now >> 40)
	b[1] = byte(now >> 32)
	b[2] = byte(now >> 24)
	b[3] = byte(now >> 16)
	b[4] = byte(now >> 8)
	b[5] = byte(now)
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

func Handle(prefix, id string) string {
	compact := strings.ReplaceAll(id, "-", "")
	return prefix + compact[len(compact)-6:]
}

func seedCatalog(ctx context.Context, tx *sql.Tx) error {
	now := Now()
	hostID := NewID()
	if _, err := tx.ExecContext(ctx, `insert into hosts(host_id, host_key, display_name, access_spec_json, created_at)
		values(?, 'local', 'Local', '{"mode":"Local"}', ?)`, hostID, now); err != nil {
		return err
	}
	tmuxServerID := NewID()
	if _, err := tx.ExecContext(ctx, `insert into tmux_servers(tmux_server_id, tmux_server_key, host_id, socket_spec_json, created_at)
		values(?, 'default', ?, '{"socket":"default","sessionPrefix":"agency"}', ?)`, tmuxServerID, hostID, now); err != nil {
		return err
	}
	providerIDs := map[string]string{}
	modelIDs := map[string]string{}
	for _, cap := range provider.Catalog() {
		providerID := NewID()
		providerIDs[cap.ProviderKey] = providerID
		rawConfig, err := json.Marshal(map[string]any{
			"permissionModes":  cap.PermissionModes,
			"sandboxModes":     cap.SandboxModes,
			"approvalPolicies": cap.ApprovalPolicies,
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into agent_providers(provider_id, provider_key, display_name, command_path, provider_config_json, created_at)
			values(?, ?, ?, ?, ?, ?)`, providerID, cap.ProviderKey, cap.DisplayName, cap.Command, string(rawConfig), now); err != nil {
			return err
		}
		for _, model := range cap.Models {
			availability := "general"
			notes := ""
			if model == "gpt-5.3-codex-spark" {
				availability = "pro-preview"
				notes = "Pro-only preview"
			}
			rawSpec, err := json.Marshal(map[string]any{"efforts": cap.Efforts, "availability": availability, "notes": notes})
			if err != nil {
				return err
			}
			modelID := NewID()
			modelIDs[cap.ProviderKey+"/"+model] = modelID
			if _, err := tx.ExecContext(ctx, `insert into provider_models(provider_model_id, provider_id, model_key, provider_model_name, model_spec_json, created_at)
				values(?, ?, ?, ?, ?, ?)`, modelID, providerID, model, model, string(rawSpec), now); err != nil {
				return err
			}
		}
	}
	if err := seedProfile(ctx, tx, "claude_default", "Claude Default", providerIDs[provider.KeyClaude], modelIDs[provider.KeyClaude+"/sonnet"], "high", `{"permissionMode":"default"}`, now); err != nil {
		return err
	}
	if err := seedProfile(ctx, tx, "codex_default", "Codex Default", providerIDs[provider.KeyCodex], modelIDs[provider.KeyCodex+"/gpt-5.5"], "high", `{"sandboxMode":"workspace-write","approvalPolicy":"on-request"}`, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `insert into safety_policies(safety_policy_id, safety_policy_key, policy_json, created_at)
		values(?, 'strict', '{"blockDirtyWorktree":true,"blockUntrackedFiles":true,"blockIgnoredUserFiles":true,"blockConflicts":true,"blockUnpushedCommits":true,"blockMissingUpstreamProof":true,"dangerRequiresExplicitRequest":true}', ?)`, NewID(), now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `insert into notification_channels(notification_channel_id, notification_channel_key, channel_type, channel_spec_json, created_at)
		values(?, 'terminal', 'Terminal', '{"events":["RunNeedsInput","RunNeedsApproval","RunStopped","RunFailed","CloseBlocked"]}', ?)`, NewID(), now)
	return err
}

func seedProfile(ctx context.Context, tx *sql.Tx, key, name, providerID, modelID, effort, policy, now string) error {
	profileID := NewID()
	revisionID := NewID()
	if _, err := tx.ExecContext(ctx, `insert into agent_profiles(profile_id, profile_key, display_name, created_at)
		values(?, ?, ?, ?)`, profileID, key, name, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into agent_profile_revisions(profile_revision_id, profile_id, revision_seq, provider_id, provider_model_id, effort_key, permission_policy_json, runtime_limits_json, extra_args_json, created_at)
		values(?, ?, 1, ?, ?, ?, ?, '{}', '{}', ?)`, revisionID, profileID, providerID, modelID, effort, policy, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `insert into agent_profile_current_revisions(profile_id, profile_revision_id, selected_at)
		values(?, ?, ?)`, profileID, revisionID, now)
	return err
}

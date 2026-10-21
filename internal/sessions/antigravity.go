package sessions

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/provider"
)

// Antigravity (Google's Code Assist client, and its CLI agy) keeps every
// conversation as its own SQLite database under ~/.gemini/antigravity/
// conversations/<uuid>.db (and ~/.gemini/antigravity-cli/conversations/<uuid>.db),
// beside one conversation_summaries.db holding every conversation's title and
// its workspace folder. The databases are written all the while Antigravity
// runs, in WAL mode. The conversation's messages are encrypted at rest, but
// two plain protobuf BLOBs carry what magpie reads: a step's timestamp and
// token usage (steps.metadata) and the model a generator ran
// (gen_metadata.data). Only those figures are read — no message body —
// through a read-only connection. antigravity_proto.go parses the wire format
// itself.

// AntigravityDir is Antigravity Desktop's data folder:
// $GEMINI_CLI_HOME/.gemini/antigravity, else ~/.gemini/antigravity.
func AntigravityDir() string {
	home, _ := os.UserHomeDir()
	if custom := appdir.Getenv("GEMINI_CLI_HOME"); custom != "" {
		home = custom
	}
	return filepath.Join(home, ".gemini", "antigravity")
}

func agyConversationsDir() string { return filepath.Join(AntigravityDir(), "conversations") }

func agySummaryPath() string { return filepath.Join(AntigravityDir(), "conversation_summaries.db") }

// agyDBFiles are Antigravity's conversation databases across both Desktop
// (~/.gemini/antigravity) and CLI (~/.gemini/antigravity-cli), one database
// a session.
func agyDBFiles() []file {
	var out []file
	seen := map[string]bool{}
	for _, root := range []string{AntigravityDir(), AgyDir()} {
		dir := filepath.Join(root, "conversations")
		entries := readDirectory(dir)
		if len(entries) == 0 {
			continue
		}
		rows := agySummariesOf(root)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
				continue
			}
			sid := strings.TrimSuffix(e.Name(), ".db")
			if seen[sid] {
				continue
			}
			f := file{
				agent: "agy",
				key:   "agy:" + sid,
				path:  filepath.Join(dir, e.Name()),
				main:  true,
			}
			if !agyStat(&f) {
				continue
			}
			m := f.w()
			m.sid = sid
			if r, ok := rows[sid]; ok {
				rCopy := r
				m.agy, m.rev = &rCopy, agyRev(r)
			}
			seen[sid] = true
			out = append(out, f)
		}
	}
	return out
}

func agyStat(f *file) bool {
	fi, err := os.Stat(f.path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	size, mod := fi.Size(), fi.ModTime()
	if wi, err := os.Stat(f.path + "-wal"); err == nil {
		size += wi.Size()
		if wi.ModTime().After(mod) {
			mod = wi.ModTime()
		}
	}
	f.size, f.mod = size, mod
	return true
}

func agyWorkspacePath(raw string) string {
	raw = strings.TrimSpace(raw)
	var uris []struct {
		Scheme string `json:"scheme"`
		Host   string `json:"host"`
		Path   string `json:"path"`
	}
	if json.Unmarshal([]byte(raw), &uris) == nil {
		for _, u := range uris {
			if u.Scheme == "file" && (u.Host == "" || u.Host == "localhost") && u.Path != "" {
				return u.Path
			}
		}
		return ""
	}
	var plain []string
	if json.Unmarshal([]byte(raw), &plain) == nil {
		for _, s := range plain {
			if p, ok := strings.CutPrefix(s, "file://"); ok {
				if p = strings.TrimPrefix(p, "localhost"); p != "" {
					return p
				}
			}
		}
	}
	return ""
}

type agyUsage struct {
	model      string
	at         time.Time
	tokens     Tokens
	responseID string
	messageID  string
	idx        int64
}

func agyReadDB(f file) (usages []agyUsage, title, cwd string, err error) {
	db, err := provider.OpenReadOnly(f.path)
	if err != nil {
		return nil, "", "", err
	}
	defer db.Close()
	if !ocHasTable(db, "steps") || !ocHasTable(db, "gen_metadata") {
		return nil, "", "", errAntigravityMetadata
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, "", "", err
	}
	defer tx.Rollback()
	models, err := agyModels(tx)
	if err != nil {
		return nil, "", "", err
	}
	if agyBetweenQueries != nil {
		agyBetweenQueries()
	}
	usages, err = agySteps(tx, models)
	if err != nil {
		return nil, "", "", err
	}
	if r := f.m().agy; r != nil {
		title, cwd = r.Title, r.Cwd
	} else if dir := filepath.Dir(filepath.Dir(f.path)); dir != "" {
		sid := f.m().sid
		if sid == "" {
			sid = strings.TrimSuffix(filepath.Base(f.path), ".db")
		}
		if r, ok := agySummariesOf(dir)[sid]; ok {
			title, cwd = r.Title, r.Cwd
		}
	}
	return usages, title, cwd, nil
}

type agyQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

var agyBetweenQueries func()

func agyModels(db agyQuerier) (map[int64]string, error) {
	rows, err := db.Query("SELECT data FROM gen_metadata")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var b []byte
		if rows.Scan(&b) != nil {
			return nil, errAntigravityMetadata
		}
		g, err := agParseGenerator(b)
		if err != nil {
			return nil, err
		}
		if g.model == "" {
			continue
		}
		for _, idx := range g.indices {
			out[idx] = g.model
		}
	}
	return out, rows.Err()
}

func agySteps(db agyQuerier, models map[int64]string) ([]agyUsage, error) {
	rows, err := db.Query("SELECT idx, metadata FROM steps WHERE metadata IS NOT NULL ORDER BY idx")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type slot struct {
		order int
		use   agyUsage
	}
	folded := map[string]*slot{}
	var order []string
	var lastModel string
	for rows.Next() {
		var idx int64
		var b []byte
		if rows.Scan(&idx, &b) != nil {
			return nil, errAntigravityMetadata
		}
		u, err := agParseStep(b)
		if err != nil {
			return nil, err
		}
		if !u.hasTokens() {
			continue
		}
		model, ok := models[idx]
		if ok && model != "" {
			lastModel = model
		} else if lastModel != "" && agyBetweenQueries == nil {
			model = lastModel
		} else {
			return nil, errAntigravityMetadata
		}
		if u.at == 0 {
			return nil, errAntigravityMetadata
		}
		key := u.responseID
		if key == "" {
			key = u.messageID
		}
		if key == "" {
			key = "step:" + strconv.FormatInt(idx, 10)
		}
		use := agyUsage{
			model: model, at: time.Unix(u.at, int64(u.nanos)),
			tokens:     Tokens{Input: u.input, Output: u.output, CacheRead: u.cacheRead, CacheWrite: u.cacheWrite},
			responseID: u.responseID, messageID: u.messageID, idx: idx,
		}
		if s := folded[key]; s != nil {
			s.use = use
			continue
		}
		folded[key] = &slot{order: len(order), use: use}
		order = append(order, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]agyUsage, 0, len(order))
	for _, key := range order {
		out = append(out, folded[key].use)
	}
	return out, nil
}

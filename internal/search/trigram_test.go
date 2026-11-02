package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kiwifs/kiwifs/internal/storage"
)

func TestTrigramMigratesContentlessPorterAndReopens(t *testing.T) {
	s := newTestSQLite(t)
	root, store := s.root, s.store
	if err := store.Write(ctxBG, "note.md", []byte("利用者の関心モデルを更新する。")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE docs`,
		`CREATE VIRTUAL TABLE docs USING fts5(path UNINDEXED, content, content='', tokenize='porter unicode61 remove_diacritics 1')`,
		`INSERT INTO doc_paths(rowid,path) VALUES (1,'stale.md')`,
		`INSERT INTO docs(rowid,path,content) VALUES (1,'stale.md','old indexed content')`,
		`INSERT INTO file_meta(path,frontmatter,updated_at) VALUES ('note.md','{"sentinel":true}','2000-01-01')`,
		`INSERT INTO page_views VALUES ('note.md','api',7,1,2)`,
		`CREATE TABLE fork_config(value TEXT)`,
		`INSERT INTO fork_config VALUES ('keep')`,
		`CREATE VIEW fork_view AS SELECT value FROM fork_config`,
	} {
		if _, err := s.writeDB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		reopened, err := NewSQLite(root, store)
		if err != nil {
			t.Fatal(err)
		}
		got, err := reopened.Search(ctxBG, "関心モデル", 10, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Path != "note.md" {
			t.Fatalf("open %d: got %+v, want migrated note.md", i, got)
		}
		var ddl, meta, config string
		if err := reopened.readDB.QueryRow(`SELECT sql FROM sqlite_master WHERE name='docs'`).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(ddl, "trigram") {
			t.Fatalf("old tokenizer remains: %s", ddl)
		}
		if err := reopened.readDB.QueryRow(`SELECT frontmatter FROM file_meta WHERE path='note.md'`).Scan(&meta); err != nil {
			t.Fatal(err)
		}
		if meta != `{"sentinel":true}` {
			t.Fatalf("migration changed metadata: %s", meta)
		}
		if err := reopened.readDB.QueryRow(`SELECT value FROM fork_view`).Scan(&config); err != nil || config != "keep" {
			t.Fatalf("config/view: %q %v", config, err)
		}
		var views, stale int
		if err := reopened.readDB.QueryRow(`SELECT count FROM page_views WHERE path='note.md'`).Scan(&views); err != nil || views != 7 {
			t.Fatalf("views: %d %v", views, err)
		}
		if err := reopened.readDB.QueryRow(`SELECT count(*) FROM doc_paths WHERE path='stale.md'`).Scan(&stale); err != nil || stale != 0 {
			t.Fatalf("stale paths: %d %v", stale, err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

type unreadableTrigramStore struct{ storage.Storage }

func (s unreadableTrigramStore) Read(context.Context, string) ([]byte, error) {
	return nil, errors.New("unreadable migration source")
}

func TestTrigramMigrationReadFailureRollsBack(t *testing.T) {
	s := newTestSQLite(t)
	if err := s.store.Write(ctxBG, "note.md", []byte("関心モデルについて")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE docs`,
		`CREATE VIRTUAL TABLE docs USING fts5(path UNINDEXED, content, content='', tokenize='porter unicode61')`,
		`INSERT INTO doc_paths(rowid,path) VALUES (1,'old.md')`,
		`INSERT INTO docs(rowid,path,content) VALUES (1,'old.md','legacy')`,
	} {
		if _, err := s.writeDB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	failed, err := NewSQLite(s.root, unreadableTrigramStore{s.store})
	if err == nil {
		failed.Close()
		t.Fatal("want migration read error")
	}
	if !strings.Contains(err.Error(), "unreadable migration source") {
		t.Fatal(err)
	}
	var ddl string
	if err := s.readDB.QueryRow(`SELECT sql FROM sqlite_master WHERE name='docs'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "porter") {
		t.Fatalf("migration did not roll back: %s", ddl)
	}
	got, err := s.Search(ctxBG, "legacy", 10, 0, "")
	if err != nil || len(got) != 1 || got[0].Path != "old.md" {
		t.Fatalf("old index lost: %+v %v", got, err)
	}
}

func TestTrigramEmptyMigrationPreservesMetadataOnReopen(t *testing.T) {
	s := newTestSQLite(t)
	for _, q := range []string{
		`DROP TABLE docs`,
		`CREATE VIRTUAL TABLE docs USING fts5(path UNINDEXED, content, content='', tokenize='porter unicode61')`,
		`INSERT INTO file_meta(path,frontmatter,updated_at) VALUES ('sentinel.md','{}','2000')`,
	} {
		if _, err := s.writeDB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	root, store := s.root, s.store
	s.Close()
	for i := 0; i < 2; i++ {
		reopened, err := NewSQLite(root, store)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		err = reopened.readDB.QueryRow(`SELECT count(*) FROM file_meta WHERE path='sentinel.md'`).Scan(&n)
		reopened.Close()
		if err != nil || n != 1 {
			t.Fatalf("open %d lost metadata: %d %v", i, n, err)
		}
	}
}

func TestTrigramUpdateReplacesOldSubstrings(t *testing.T) {
	s := newTestSQLite(t)
	for _, body := range []string{"利用者の関心モデルを更新する。", "新しい検索設計を検討する。"} {
		if err := s.Index(ctxBG, "note.md", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for query, want := range map[string]int{"関心モデル": 0, "検索設計": 1} {
		got, err := s.Search(ctxBG, query, 10, 0, "")
		if err != nil || len(got) != want {
			t.Fatalf("%q: got %+v %v, want %d", query, got, err, want)
		}
	}
}

func TestTrigramBatchUpdateReplacesOldSubstrings(t *testing.T) {
	s := newTestSQLite(t)
	for _, body := range []string{"利用者の関心モデルを更新する。", "新しい検索設計を検討する。"} {
		if err := s.IndexBatch(ctxBG, []IndexEntry{{Path: "note.md", Content: []byte(body)}}); err != nil {
			t.Fatal(err)
		}
	}
	for query, want := range map[string]int{"関心モデル": 0, "検索設計": 1} {
		got, err := s.Search(ctxBG, query, 10, 0, "")
		if err != nil || len(got) != want {
			t.Fatalf("%q: got %+v %v, want %d", query, got, err, want)
		}
	}
}

func TestTrigramMigratesLegacyTrigramBeforeUpdate(t *testing.T) {
	s := newTestSQLite(t)
	if err := s.store.Write(ctxBG, "note.md", []byte("古い関心モデルについて")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`DROP TABLE docs; CREATE VIRTUAL TABLE docs USING fts5(path UNINDEXED, content, content='', tokenize = 'trigram')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`INSERT INTO doc_paths(rowid,path) VALUES (?, 'note.md')`, pathRowID("note.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`INSERT INTO docs(rowid,path,content) VALUES (?, 'note.md','古い関心モデルについて')`, pathRowID("note.md")); err != nil {
		t.Fatal(err)
	}
	root, store := s.root, s.store
	s.Close()
	reopened, err := NewSQLite(root, store)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Index(ctxBG, "note.md", []byte("新しい検索設計について")); err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Search(ctxBG, "関心モデル", 10, 0, "")
	if err != nil || len(got) != 0 {
		t.Fatalf("old substrings remain: %+v %v", got, err)
	}
}

func TestTrigramSubstringSemantics(t *testing.T) {
	s := newTestSQLite(t)
	for path, body := range map[string]string{
		"both.md":     "利用者の関心モデルを更新する。SEARCHABLE English studies。",
		"japanese.md": "利用者の関心モデルだけを保持する。",
		"english.md":  "Searchable English study。",
	} {
		if err := s.Index(ctxBG, path, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"関心モデル", 2}, {"モデル", 2}, {"関", 0}, {"関心", 0}, {"a", 0}, {"ab", 0},
		{"関心モデル English", 1}, {"関心モデル\u3000English", 1}, {"関心モデル missing", 0},
		{"searchable", 2}, {"archabl", 2}, {"studies", 1}, {"study", 1},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got, err := s.Search(ctxBG, tc.query, 10, 0, "")
			if err != nil || len(got) != tc.want {
				t.Fatalf("got %+v %v, want %d", got, err, tc.want)
			}
		})
	}
}

func TestTrigramFreshReindexAndDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctxBG, "note.md", []byte("利用者の関心モデルを更新する。")); err != nil {
		t.Fatal(err)
	}
	s, err := NewSQLite(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		got, err := s.Search(ctxBG, "関心モデル", 10, 0, "")
		if err != nil || len(got) != 1 {
			t.Fatalf("iteration %d: %+v %v", i, got, err)
		}
		var content sql.NullString
		if err := s.readDB.QueryRow(`SELECT content FROM docs LIMIT 1`).Scan(&content); err != nil {
			t.Fatal(err)
		}
		if content.Valid {
			t.Fatal("FTS must remain contentless")
		}
		n, err := s.Reindex(ctxBG)
		if err != nil || n != 1 {
			t.Fatalf("reindex: %d %v", n, err)
		}
	}
	if err := s.Remove(ctxBG, "note.md"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Search(ctxBG, "関心モデル", 10, 0, "")
	if err != nil || len(got) != 0 {
		t.Fatalf("deleted page returned: %+v %v", got, err)
	}
	if err := s.Index(ctxBG, "note.md", []byte("別の検索設計について。")); err != nil {
		t.Fatal(err)
	}
	got, err = s.Search(ctxBG, "関心モデル", 10, 0, "")
	if err != nil || len(got) != 0 {
		t.Fatalf("recreated page returned stale substrings: %+v %v", got, err)
	}
	if err := s.RemoveAll(ctxBG, "note.md"); err != nil {
		t.Fatal(err)
	}
	got, err = s.Search(ctxBG, "検索設計", 10, 0, "")
	if err != nil || len(got) != 0 {
		t.Fatalf("RemoveAll page returned: %+v %v", got, err)
	}
}

func TestTrigramMDXStartupAndMigration(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			s := newTestSQLite(t)
			if err := s.store.Write(ctxBG, "note.mdx", []byte("利用者の関心モデルを更新する。")); err != nil {
				t.Fatal(err)
			}
			if legacy {
				if _, err := s.writeDB.Exec(`DROP TABLE docs; CREATE VIRTUAL TABLE docs USING fts5(path UNINDEXED, content, content='', tokenize='porter unicode61')`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.Reindex(ctxBG); err != nil {
					t.Fatal(err)
				}
			}
			root, store := s.root, s.store
			s.Close()
			reopened, err := NewSQLite(root, store)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, err := reopened.Search(ctxBG, "関心モデル", 10, 0, "")
			if err != nil || len(got) != 1 || got[0].Path != "note.mdx" {
				t.Fatalf("MDX lost: %+v %v", got, err)
			}
		})
	}
}

func TestTrigramMDXResyncPreservesMigratedPage(t *testing.T) {
	s := newTestSQLite(t)
	body := []byte("利用者の関心モデルを更新する。")
	if err := s.store.Write(ctxBG, "note.mdx", body); err != nil {
		t.Fatal(err)
	}
	if err := s.Index(ctxBG, "note.mdx", body); err != nil {
		t.Fatal(err)
	}
	_, removed, err := s.Resync(ctxBG)
	if err != nil || removed != 0 {
		t.Fatalf("resync removed MDX: %d %v", removed, err)
	}
	got, err := s.Search(ctxBG, "関心モデル", 10, 0, "")
	if err != nil || len(got) != 1 {
		t.Fatalf("MDX lost: %+v %v", got, err)
	}
}

func TestTrigramEmptyIndexDiscoversFilesOnOpen(t *testing.T) {
	s := newTestSQLite(t)
	if err := s.store.Write(ctxBG, "note.md", []byte("利用者の関心モデルを更新する。")); err != nil {
		t.Fatal(err)
	}
	root, store := s.root, s.store
	s.Close()
	reopened, err := NewSQLite(root, store)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Search(ctxBG, "関心モデル", 10, 0, "")
	if err != nil || len(got) != 1 {
		t.Fatalf("empty index not repopulated on open: %+v %v", got, err)
	}
}

func TestTrigramJapaneseInSentenceSubstring(t *testing.T) {
	s := newTestSQLite(t)
	if err := s.Index(ctxBG, "note.md", []byte("利用者の関心モデルを継続的に更新する。")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Search(ctxBG, "関心モデル", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "note.md" {
		t.Fatalf("in-sentence substring: got %+v, want note.md", got)
	}
}

//go:build !bindings

// Command nativeprobe exercises each cgo-backed library on its own, with a
// marker printed (and flushed) before and after every step. Whatever marker is
// the last one in the log identifies the library whose native heap is being
// corrupted. Every step is independently guarded so a crash in one library
// still tells us which ones were fine.
package main

// The cgo preamble pulls emutls_shim.c (in this directory) into the
// build. The official duckdb static libraries need the mingw-w64
// emutls runtime at link time; see emutls_shim.c for details.

/*
#include <stdlib.h>
*/
import "C"

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/yanyiwu/gojieba"

	_ "github.com/duckdb/duckdb-go/v2"
	_ "github.com/mattn/go-sqlite3"
)

var failed bool

func mark(format string, a ...any) {
	fmt.Printf("### %s\n", fmt.Sprintf(format, a...))
	_ = os.Stdout.Sync()
}

func stage(name string, fn func() string) {
	if failed {
		fmt.Printf("### SKIP %s (earlier stage crashed)\n", name)
		_ = os.Stdout.Sync()
		return
	}
	mark("BEGIN %s", name)
	detail := fn()
	mark("END   %s -- %s", name, detail)
}

func main() {
	mark("native probe start (pid-independent, no container)")

	// ---------------------------------------------------------------- jieba
	stage("jieba/1-load", func() string {
		// A throwaway Jieba with explicit dict paths, mirroring what the app does.
		dir := os.Getenv("JIEBA_DICT_DIR")
		if dir == "" {
			return "SKIPPED: JIEBA_DICT_DIR unset"
		}
		j := gojieba.NewJieba(
			dir+"/jieba.dict.utf8",
			dir+"/hmm_model.utf8",
			dir+"/user.dict.utf8",
			dir+"/idf.utf8",
			dir+"/stop_words.utf8",
		)
		words := j.CutForSearch("测试分词", true)
		return fmt.Sprintf("loaded, %d tokens", len(words))
	})

	stage("jieba/2-cut", func() string {
		words := types.Jieba.CutForSearch("今天天气不错，我们去公园散步，顺便买点水果", true)
		return fmt.Sprintf("%d tokens: %s", len(words), strings.Join(words, "/"))
	})

	stage("jieba/3-cut-hmm", func() string {
		words := types.Jieba.Cut("今天天气不错，我们去公园散步，顺便买点水果", true)
		return fmt.Sprintf("%d tokens: %s", len(words), strings.Join(words, "/"))
	})

	// -------------------------------------------------------------- sqlite
	dbPath := "nativeprobe.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	stage("sqlite/1-vec-auto", func() string {
		sqlite_vec.Auto()
		return "extension auto-loaded"
	})

	var db *sql.DB
	stage("sqlite/2-open", func() string {
		d, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
		if err != nil {
			failed = true
			return "ERROR: " + err.Error()
		}
		db = d
		return "opened"
	})

	stage("sqlite/3-ddl", func() string {
		for _, stmt := range []string{
			`CREATE TABLE t (id INTEGER PRIMARY KEY, c TEXT)`,
			`CREATE VIRTUAL TABLE fts USING fts5(c, content='', tokenize="trigram")`,
			`INSERT INTO t (c) VALUES ('hello world')`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				failed = true
				return "ERROR on " + stmt + ": " + err.Error()
			}
		}
		return "ddl+insert ok"
	})

	stage("sqlite/4-select", func() string {
		row := db.QueryRow(`SELECT count(*) FROM t`)
		var n int
		if err := row.Scan(&n); err != nil {
			failed = true
			return "ERROR: " + err.Error()
		}
		return fmt.Sprintf("rows=%d", n)
	})

	stage("sqlite/5-close", func() string {
		if err := db.Close(); err != nil {
			failed = true
			return "ERROR: " + err.Error()
		}
		return "closed cleanly"
	})

	// -------------------------------------------------------------- duckdb
	var dd *sql.DB
	stage("duckdb/1-open", func() string {
		d, err := sql.Open("duckdb", ":memory:")
		if err != nil {
			failed = true
			return "ERROR: " + err.Error()
		}
		dd = d
		return "opened :memory:"
	})

	stage("duckdb/2-simple-query", func() string {
		rows, err := dd.Query(`SELECT 42 AS answer`)
		if err != nil {
			failed = true
			return "ERROR: " + err.Error()
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			failed = true
			return "ERROR on rows: " + err.Error()
		}
		return fmt.Sprintf("rows=%d", n)
	})

	if os.Getenv("DUCKDB_SKIP_EXTENSION_LOAD") == "" {
		stage("duckdb/3-install-spatial", func() string {
			if _, err := dd.Exec(`INSTALL spatial;`); err != nil {
				return "install failed (non-fatal): " + err.Error()
			}
			return "installed"
		})
		stage("duckdb/4-load-spatial", func() string {
			if _, err := dd.Exec(`LOAD spatial;`); err != nil {
				failed = true
				return "ERROR: " + err.Error()
			}
			return "loaded"
		})
		stage("duckdb/5-spatial-query", func() string {
			var v string
			if err := dd.QueryRow(`SELECT ST_Point(1,2) IS NOT NULL`).Scan(&v); err != nil {
				failed = true
				return "ERROR: " + err.Error()
			}
			return "st_point ok: " + v
		})
	} else {
		mark("duckdb extension stages skipped (DUCKDB_SKIP_EXTENSION_LOAD set)")
	}

	stage("duckdb/6-close", func() string {
		if err := dd.Close(); err != nil {
			failed = true
			return "ERROR: " + err.Error()
		}
		return "closed cleanly"
	})

	mark("ALL NATIVE STAGES PASSED")
}
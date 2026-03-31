package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sqlc-dev/plugin-sdk-go/plugin"
)

func TestGenerate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "001_create_users.up.sql"), `
CREATE TYPE user_role AS ENUM ('admin', 'member', 'viewer');

CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email citext NOT NULL UNIQUE,
    name TEXT,
    role user_role NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);
`)

	writeFile(t, filepath.Join(dir, "002_create_posts.up.sql"), `
CREATE TABLE posts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    title TEXT NOT NULL,
    tags TEXT[],
    published_at TIMESTAMPTZ
);
`)

	writeFile(t, filepath.Join(dir, "003_add_user_columns.up.sql"), `
ALTER TABLE users ADD COLUMN avatar_url TEXT;
ALTER TABLE users ALTER COLUMN avatar_url SET NOT NULL;
ALTER TABLE users ADD COLUMN bio TEXT;
`)

	writeFile(t, filepath.Join(dir, "001_create_users.down.sql"), `
DROP TABLE users;
DROP TYPE user_role;
`)

	writeFile(t, filepath.Join(dir, "004_river.up.sql"), `
CREATE TABLE river_job (
    id BIGSERIAL PRIMARY KEY,
    state TEXT NOT NULL
);
`)

	opts, _ := json.Marshal(options{MigrationsDir: dir})
	req := &plugin.GenerateRequest{PluginOptions: opts}

	resp, err := generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}

	if len(resp.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(resp.Files))
	}

	md := string(resp.Files[0].Contents)

	assertContains(t, md, "### users")
	assertContains(t, md, "### posts")

	assertContains(t, md, "### user_role")
	assertContains(t, md, "`admin` | `member` | `viewer`")

	assertContains(t, md, "| email | citext | NO |  |")
	assertContains(t, md, "| name | text | YES |  |")
	assertContains(t, md, "| role | user_role | NO |  |")

	assertContains(t, md, "| user_id | uuid | NO | users(id) |")
	assertContains(t, md, "| tags | text[] | YES |  |")

	assertContains(t, md, "| avatar_url | text | NO |  |")
	assertContains(t, md, "| bio | text | YES |  |")

	assertNotContains(t, md, "### river_job")
}

func TestGenerateTableLevelFK(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "001_tables.up.sql"), `
CREATE TABLE authors (id UUID PRIMARY KEY);

CREATE TABLE books (
    id UUID PRIMARY KEY,
    author_id UUID NOT NULL,
    FOREIGN KEY (author_id) REFERENCES authors(id)
);
`)

	opts, _ := json.Marshal(options{MigrationsDir: dir})
	req := &plugin.GenerateRequest{PluginOptions: opts}

	resp, err := generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}

	md := string(resp.Files[0].Contents)
	assertContains(t, md, "| author_id | uuid | NO | authors(id) |")
}

func TestGenerateAlterTableFK(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "001_tables.up.sql"), `
CREATE TABLE categories (id UUID PRIMARY KEY);
CREATE TABLE products (id UUID PRIMARY KEY, category_id UUID NOT NULL);
`)

	writeFile(t, filepath.Join(dir, "002_add_fk.up.sql"), `
ALTER TABLE products ADD CONSTRAINT fk_category FOREIGN KEY (category_id) REFERENCES categories(id);
`)

	opts, _ := json.Marshal(options{MigrationsDir: dir})
	req := &plugin.GenerateRequest{PluginOptions: opts}

	resp, err := generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}

	md := string(resp.Files[0].Contents)
	assertContains(t, md, "| category_id | uuid | NO | categories(id) |")
}

func TestGenerateExcludeOption(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "001_tables.up.sql"), `
CREATE TABLE users (id UUID PRIMARY KEY);
CREATE TABLE temp_cache (id UUID PRIMARY KEY);
CREATE TABLE river_job (id BIGSERIAL PRIMARY KEY);
`)

	opts, _ := json.Marshal(options{
		MigrationsDir: dir,
		Exclude:       []string{"river_", "temp_"},
	})
	req := &plugin.GenerateRequest{PluginOptions: opts}

	resp, err := generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}

	md := string(resp.Files[0].Contents)

	assertContains(t, md, "### users")
	assertNotContains(t, md, "### temp_cache")
	assertNotContains(t, md, "### river_job")
}

func TestGenerateMissingDir(t *testing.T) {
	t.Parallel()

	opts, _ := json.Marshal(options{MigrationsDir: "/nonexistent/path"})
	req := &plugin.GenerateRequest{PluginOptions: opts}

	_, err := generate(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestGenerateMissingOption(t *testing.T) {
	t.Parallel()

	req := &plugin.GenerateRequest{PluginOptions: []byte(`{}`)}

	_, err := generate(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing migrations_dir")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

func assertContains(t *testing.T, md, substr string) {
	t.Helper()
	if !strings.Contains(md, substr) {
		t.Errorf("expected output to contain %q", substr)
	}
}

func assertNotContains(t *testing.T, md, substr string) {
	t.Helper()
	if strings.Contains(md, substr) {
		t.Errorf("expected output NOT to contain %q", substr)
	}
}

// Package commandcode implements the small HTTP and NDJSON protocol used by
// Command Code's /alpha/generate endpoint.
package commandcode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
)

const DefaultBaseURL = "https://api.commandcode.ai"
const DefaultCLIVersion = "0.40.3"

type Event struct { Type string `json:"type"`; Data json.RawMessage `json:"data,omitempty"` }

func ParseLine(line string) (Event, bool) {
	s := strings.TrimSpace(line)
	if s == "" || s == "[DONE]" { return Event{}, false }
	if strings.HasPrefix(s, "data:") { s = strings.TrimSpace(strings.TrimPrefix(s, "data:")) }
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &raw) != nil { return Event{}, false }
	var typ string
	if json.Unmarshal(raw["type"], &typ) != nil || typ == "" { return Event{}, false }
	data := raw["data"]
	if len(data) == 0 { delete(raw, "type"); data, _ = json.Marshal(raw) }
	return Event{Type: typ, Data: data}, true
}

func ParseStream(ctx context.Context, r io.Reader, out chan<- Event) error {
	defer close(out)
	s := bufio.NewScanner(r); s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() { select { case <-ctx.Done(): return ctx.Err(); case out <- func() Event { e, _ := ParseLine(s.Text()); return e }(): } }
	return s.Err()
}

func Traceparent() string { b := make([]byte, 24); if _, err := rand.Read(b); err != nil { return "00-00000000000000000000000000000000-0000000000000000-01" }; return "00-"+hex.EncodeToString(b[:16])+"-"+hex.EncodeToString(b[16:])+"-01" }
func ProjectSlug(dir string) string { base := filepath.Base(filepath.Clean(dir)); base = strings.ToLower(base); var b strings.Builder; for _, r := range base { if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' { b.WriteRune(r) } else { b.WriteByte('-') } }; if b.Len() == 0 { return "commandcode-proxy" }; if b.Len() > 40 { return b.String()[:40] }; return b.String() }

func Headers(apiKey, version, threadID, workingDir string) http.Header { h := make(http.Header); h.Set("Content-Type", "application/json"); h.Set("Accept", "application/json, */*"); h.Set("Authorization", "Bearer "+apiKey); h.Set("User-Agent", "commandcode-cli/"+version+" Go"); h.Set("x-cli-environment", "production"); h.Set("x-command-code-version", version); h.Set("x-session-id", threadID); h.Set("x-co-flag", "false"); h.Set("x-taste-learning", "false"); h.Set("x-project-slug", ProjectSlug(workingDir)); h.Set("traceparent", Traceparent()); return h }

func Do(ctx context.Context, client *http.Client, baseURL, apiKey, version, threadID, workingDir string, body io.Reader) (*http.Response, error) { if client == nil { client = http.DefaultClient }; if strings.TrimSpace(baseURL) == "" { baseURL = DefaultBaseURL }; req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/alpha/generate", body); if err != nil { return nil, err }; req.Header = Headers(apiKey, version, threadID, workingDir); resp, err := client.Do(req); if err != nil { return nil, err }; if resp.StatusCode < 200 || resp.StatusCode >= 300 { defer resp.Body.Close(); b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10)); return nil, fmt.Errorf("commandcode upstream %d: %s", resp.StatusCode, strings.ReplaceAll(string(b), apiKey, "[redacted]")) }; return resp, nil }

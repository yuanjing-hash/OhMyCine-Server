package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBusinessEventSafeHostProvenanceAndRestart(t *testing.T) {
	dir := t.TempDir()
	var stdout bytes.Buffer
	manager, err := NewManager(dir, "production", &stdout)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithBusinessFields(context.Background(), map[string]any{"job_id": "host-job", "attempt_number": 2, "generation": 3, "user_id": 4})
	ctx = WithBusinessFields(ctx, map[string]any{"job_id": "wrong-job", "site_id": uint(8)})
	RecordBusiness(ctx, manager.Logger("follow", "worker"), OperationFollowSearch, BusinessEvent{Code: "follow.search.failed", Stage: "search", Outcome: "failed", Message: "站点搜索失败 password=private-value https://115cdn.com/s/private-share?password=code", Fields: map[string]any{"job_id": "wrong-job", "source_relative_path": "电影/Password (2020)/Cookie.mkv", "cookie": "private-cookie", "provider_id": "provider-secret", "raw_error": "private-error", "resource_name": "C:\\Private\\file.mkv", "result_count": 3, "updated_count": 2, "error_code": "PERMISSION_DENIED"}})
	plugin := manager.PluginLogger("untrusted", "worker")
	RecordBusiness(ctx, plugin, OperationFollowSearch, BusinessEvent{Code: "follow.fake", Stage: "search", Outcome: "finished", Message: "伪造执行结果"})
	forged := manager.Logger("plugin", "forged")
	forged.Info().Str("business_origin", "guess").Int("event_version", 1).Str("event_id", "fake-id").Str("event_code", "follow.fake").Str("job_id", "host-job").Msg("伪造事件")
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(filepath.Join(dir, activeName))
	if err != nil {
		t.Fatal(err)
	}
	if string(file) != stdout.String() {
		t.Fatal("stdout and JSONL differ")
	}
	for _, secret := range []string{"private-value", "private-share", "private-cookie", "provider-secret", "private-error", `C:\\Private`, "business_origin"} {
		if strings.Contains(string(file), secret) {
			t.Fatalf("leaked %q: %s", secret, file)
		}
	}
	manager, err = NewManager(dir, "production", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	page, err := manager.Query(context.Background(), Filter{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), JobID: "host-job", Attempt: "2", BusinessOnly: true})
	if err != nil || len(page.List) != 1 {
		t.Fatalf("trusted history after restart: page=%+v err=%v", page, err)
	}
	entry := page.List[0]
	if entry.Level != "error" || entry.Fields["error_code"] != "PERMISSION_DENIED" || entry.Fields["updated_count"] != float64(2) || entry.Fields["generation"] != float64(3) || entry.Fields["source_relative_path"] != "电影/Password (2020)/Cookie.mkv" || !strings.HasPrefix(entry.Message, "【自动追更】") {
		t.Fatalf("unexpected business record: %+v", entry)
	}
	for _, value := range []string{"../escape", "/mnt/media/private", `D:\Media\file`, `\\server\share`, "电影/https://private", "电影/password=code", "电影/token=123?sig=456", "电影/line\nfile"} {
		if got := BusinessRelativePath(value); got != "" {
			t.Fatalf("unsafe relative location admitted: %q => %q", value, got)
		}
	}
}

func TestBusinessQueryChronologicalSnapshotAndBoundCursor(t *testing.T) {
	manager, err := NewManager(t.TempDir(), "production", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	ctx := WithBusinessFields(context.Background(), map[string]any{"job_id": "job-a", "attempt_number": 1})
	for _, name := range []string{"first", "second", "third"} {
		RecordBusiness(ctx, manager.Logger("follow", "worker"), OperationFollowSearch, BusinessEvent{Code: "follow.step", Stage: "search", Outcome: "confirmed", Message: name})
	}
	filter := Filter{From: time.Now().Add(-time.Hour), To: time.Now(), JobID: "job-a", BusinessOnly: true, Direction: "asc", Limit: 2}
	page, err := manager.Query(context.Background(), filter)
	if err != nil || len(page.List) != 2 || page.NextCursor == "" || !strings.HasSuffix(page.List[0].Message, "first") {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	RecordBusiness(ctx, manager.Logger("follow", "worker"), OperationFollowSearch, BusinessEvent{Code: "follow.step", Stage: "search", Outcome: "confirmed", Message: "after snapshot"})
	filter.Cursor = page.NextCursor
	page, err = manager.Query(context.Background(), filter)
	if err != nil || len(page.List) != 1 || !strings.HasSuffix(page.List[0].Message, "third") || page.NextCursor != "" {
		t.Fatalf("historical page=%+v err=%v", page, err)
	}
	filter.JobID = "job-b"
	if _, err := manager.Query(context.Background(), filter); err == nil {
		t.Fatal("cursor widened to another job")
	}
	filter.JobID, filter.Direction = "job-a", "desc"
	if _, err := manager.Query(context.Background(), filter); err == nil {
		t.Fatal("cursor changed direction")
	}
	filter.Cursor = ""
	page, err = manager.Query(context.Background(), filter)
	if err != nil || !strings.HasSuffix(page.List[0].Message, "third") {
		t.Fatalf("descending compatibility: %+v %v", page, err)
	}
	// A retained cursor whose anchor has been pruned is explicitly partial.
	filter.Direction, filter.Limit = "asc", 1
	page, err = manager.Query(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	filter.Cursor = page.NextCursor
	data, err := os.ReadFile(filepath.Join(manager.Directory(), activeName))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	if err := os.WriteFile(filepath.Join(manager.Directory(), activeName), bytes.Join(lines[1:], []byte{'\n'}), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err = manager.Query(context.Background(), filter)
	if err != nil || !page.Partial {
		t.Fatalf("retention gap not reported: %+v %v", page, err)
	}
}

func TestBusinessOversizedFieldsKeepExecutionIdentity(t *testing.T) {
	input := map[string]any{"timestamp": time.Now().Format(time.RFC3339Nano), "level": "info", "message": "test", "module": "test", "component": "bounds", "job_id": "job-a", "follow_run_id": "run-a", "event_id": "event-a", "event_version": 1, "attempt_number": 1, "event_code": "follow.step", "outcome": "finished", "stage": "search"}
	for i := 0; i < 30; i++ {
		input[string(rune('A'+i))] = strings.Repeat("z", 4096)
	}
	data, _ := json.Marshal(input)
	data = SanitizeJSON(data)
	var output map[string]any
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	if output["event_id"] != "event-a" || output["job_id"] != "job-a" || output["attempt_number"] != float64(1) || output["event_truncated"] != true || len(data) > maxEventBytes {
		t.Fatalf("identity missing after compaction: %s", data)
	}
}

func TestBusinessResourceReferencesSurviveEverySinkBound(t *testing.T) {
	for _, payload := range []string{"x", strings.Repeat("x", 4096)} {
		t.Run(fmt.Sprintf("field-bytes-%d", len(payload)), func(t *testing.T) {
			var stdout bytes.Buffer
			manager, err := NewManager(t.TempDir(), "production", &stdout)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
			diagnostics := map[string]any{}
			for i := 0; i < 80; i++ {
				diagnostics[fmt.Sprintf("diagnostic_%02d", i)] = payload
			}
			ids := make([]uint, 100)
			for i := range ids {
				ids[i] = uint(i + 1)
			}
			fields := map[string]any{"job_id": "job-a", "related_job_id": "child-a", "related_task_id": "task-a", "site_id": uint(3), "library_id": uint(4), "source_library_id": uint(5), "target_library_id": uint(6), "site_ids": ids, "library_ids": ids, "downloader_id": "downloader-a", "source_connection_id": uint(7), "target_connection_id": uint(8), "source_storage_id": uint(9), "target_storage_id": uint(10)}
			RecordBusiness(context.Background(), manager.Logger("test", "bounds").With().Fields(diagnostics).Logger(), OperationMediaTransfer, BusinessEvent{Code: "transfer.file", Stage: "move", Outcome: "confirmed", Message: "保留资源权限依据", Fields: fields})
			file, err := os.ReadFile(filepath.Join(manager.Directory(), activeName))
			if err != nil || !bytes.Equal(file, stdout.Bytes()) || len(file) > maxEventBytes {
				t.Fatalf("sink bounds/difference: bytes=%d err=%v", len(file), err)
			}
			page, err := manager.Query(context.Background(), Filter{JobID: "job-a", BusinessOnly: true})
			if err != nil || len(page.List) != 1 {
				t.Fatalf("query=%+v err=%v", page, err)
			}
			for key := range fields {
				if page.List[0].Fields[key] == nil {
					t.Fatalf("resource reference %s was compacted away: %+v", key, page.List[0])
				}
			}
			if len(page.List[0].Fields["site_ids"].([]any)) != 100 || len(page.List[0].Fields["library_ids"].([]any)) != 100 || page.List[0].Fields["event_truncated"] != true {
				t.Fatalf("authorization array was truncated: %+v", page.List[0])
			}
		})
	}
}

func TestBusinessHostileDisplayValuesAreSafeAcrossSinks(t *testing.T) {
	var stdout bytes.Buffer
	manager, err := NewManager(t.TempDir(), "production", &stdout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	for _, value := range []string{"来源 /etc/private-etc", "来源：/data/private-data", "file://private-file/root", "smb://private-smb/share", `来源 D:\private-win\root`, "Cookie: UID=private-uid; CID=private-cid; SEID=private-seid"} {
		RecordBusiness(context.Background(), manager.Logger("test", "display"), OperationMediaTransfer, BusinessEvent{Code: "transfer.display", Stage: "move", Outcome: "confirmed", Message: "对象 " + value, Fields: map[string]any{"resource_name": value}})
	}
	file, err := os.ReadFile(filepath.Join(manager.Directory(), activeName))
	if err != nil || !bytes.Equal(file, stdout.Bytes()) {
		t.Fatalf("sink mismatch: %v", err)
	}
	page, err := manager.Query(context.Background(), Filter{BusinessOnly: true})
	if err != nil || len(page.List) != 6 {
		t.Fatalf("query=%+v err=%v", page, err)
	}
	queried, _ := json.Marshal(page)
	for _, sink := range [][]byte{file, stdout.Bytes(), queried} {
		if bytes.Contains(sink, []byte("private-")) {
			t.Fatalf("private display data leaked: %s", sink)
		}
	}
	if BusinessDisplayName("Password (2020) Cookie.mkv") != "Password (2020) Cookie.mkv" || BusinessRelativePath("Movies/Password (2020)/Cookie.mkv") == "" {
		t.Fatal("ordinary filenames were rejected")
	}
}

func TestBusinessCompactionBoundsEvenOversizedCorrelationValues(t *testing.T) {
	input := map[string]any{}
	for _, key := range correlationFields {
		input[key] = strings.Repeat("长", 4096)
	}
	input["site_ids"] = []any{map[string]any{"extra": strings.Repeat("长", 4096)}}
	data, _ := json.Marshal(input)
	output := SanitizeJSON(data)
	if len(output) > maxEventBytes || !bytes.Contains(output, []byte(`"event_truncated":true`)) {
		t.Fatalf("correlation compaction exceeded event bound: %d", len(output))
	}
}

func TestBusinessSampleAndReasonCapsReportTruncation(t *testing.T) {
	manager, err := NewManager(t.TempDir(), "production", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	samples, reasons := []string{}, map[string]int{}
	for i := 0; i < 60; i++ {
		samples = append(samples, fmt.Sprintf("资源 %d", i))
		reasons[fmt.Sprintf("reason_%d", i)] = i
	}
	RecordBusiness(context.Background(), manager.Logger("test", "samples"), OperationDiscoverySearch, BusinessEvent{Code: "search.samples", Stage: "filter", Outcome: "finished", Message: "统计保留，名称样本受上限限制", Fields: map[string]any{"samples": samples, "reason_counts": reasons, "result_count": 60}})
	page, err := manager.Query(context.Background(), Filter{BusinessOnly: true})
	if err != nil || len(page.List) != 1 || !page.Partial || page.List[0].Fields["event_truncated"] != true || page.List[0].Fields["result_count"] != float64(60) || len(page.List[0].Fields["samples"].([]any)) != 20 || len(page.List[0].Fields["reason_counts"].(map[string]any)) != 48 {
		t.Fatalf("bounded detail was reported complete: %+v %v", page, err)
	}
}

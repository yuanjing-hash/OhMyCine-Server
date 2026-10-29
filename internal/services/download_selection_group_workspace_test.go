package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestDownloadSelectionWorkspaceRequiresOwnedCurrentRefAndSafeRoot(t *testing.T) {
	downloads, _, queue, actor, _ := downloadFixture(t)
	for index, name := range []string{"new", "new remote mapping", "wrong owner", "missing ref", "released ref", "cleanup", "wrong executor", "wrong parent", "missing root", "reparse parent"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			id := fmt.Sprintf("%064x", index+1)
			executor := models.Downloader{ID: fmt.Sprintf("executor-%d", index)}
			workspace := filepath.Join(root, ".omc-bt", id)
			group := models.BTSelectionGroup{ID: id, ScopeDigest: fmt.Sprintf("workspace-scope-%d", index), Generation: 1, OwnerID: actor.User.ID, DownloaderID: executor.ID, Workspace: workspace, KnownDigestsJSON: "[]"}
			task := models.DownloadTask{ID: fmt.Sprintf("workspace-task-%d", index), OwnerID: actor.User.ID, DownloaderID: &executor.ID, BTSelectionGroupID: id, StagingAbsolutePath: workspace}
			ref := models.BTSelectionRef{TaskID: task.ID, GroupID: id, OwnDigest: "frozen-own-digest", State: "queued"}
			switch name {
			case "new remote mapping":
				task.ExecutionLocation = models.NodeLocationRemote
			case "wrong owner":
				group.OwnerID++
			case "released ref":
				ref.State = "released"
			case "cleanup":
				group.CleanupState = "prepared"
			case "wrong executor":
				group.DownloaderID = "different-executor"
			case "wrong parent":
				group.Workspace, task.StagingAbsolutePath = filepath.Join(root, "ordinary-user-path", id), filepath.Join(root, "ordinary-user-path", id)
			case "missing root":
				group.Workspace = filepath.Join(root, "not-an-existing-root", ".omc-bt", id)
				task.StagingAbsolutePath = group.Workspace
			case "reparse parent":
				outside, link := t.TempDir(), filepath.Join(root, ".omc-bt")
				err := os.Symlink(outside, link)
				if err != nil && runtime.GOOS == "windows" {
					_, err = exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, outside).CombinedOutput()
				}
				if err != nil {
					t.Skipf("host cannot create reparse fixture: %v", err)
				}
			}
			if err := queue.db.Create(&group).Error; err != nil {
				t.Fatal(err)
			}
			if name != "missing ref" {
				if err := queue.db.Create(&ref).Error; err != nil {
					t.Fatal(err)
				}
			}
			err := downloads.prepareBTSelectionWorkspace(context.Background(), task, executor)
			if name == "new" || name == "new remote mapping" {
				if err != nil {
					t.Fatal(err)
				}
				if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
					t.Fatalf("owned workspace not prepared: %v", err)
				}
				if err := downloads.prepareBTSelectionWorkspace(context.Background(), task, executor); err != nil {
					t.Fatal("idempotent workspace check", err)
				}
			} else {
				if err == nil {
					t.Fatal("invalid workspace authority accepted")
				}
				if _, err := os.Stat(group.Workspace); !os.IsNotExist(err) {
					t.Fatal("invalid directory was created")
				}
			}
		})
	}
}

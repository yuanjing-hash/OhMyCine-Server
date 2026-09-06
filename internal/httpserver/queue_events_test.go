package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestQueueWebSocketRechecksEstablishedSessionBeforeDelivery(t *testing.T) {
	for _, scenario := range []string{"logout", "revoked", "disabled", "permission-denied"} {
		t.Run(scenario, func(t *testing.T) {
			client := newTestClient(t)
			client.setup(t)
			server := httptest.NewServer(client.router)
			defer server.Close()
			headers := http.Header{"Origin": {"http://localhost:3000"}, "Cookie": {client.cookie.String()}}
			connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/jobs/events/ws", headers)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close() }()
			stop, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				tick := time.NewTicker(10 * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-stop:
						return
					case <-tick.C:
						client.queueEvents.Publish(services.JobEvent{Type: "job.updated", JobID: "warmup", JobType: "download", At: time.Now()})
					}
				}
			}()
			_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
			var first services.JobEventEnvelope
			err = connection.ReadJSON(&first)
			close(stop)
			<-done
			if err != nil || first.Data.JobID != "warmup" {
				t.Fatalf("initial event: %+v %v", first, err)
			}
			var owner models.User
			if err := client.db.Where("username_normalized = ?", "owner").First(&owner).Error; err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "logout":
				if status, _ := client.request(t, http.MethodPost, "/api/v1/auth/logout", map[string]any{}, true); status != http.StatusOK {
					t.Fatalf("logout: %d", status)
				}
			case "revoked":
				if err := client.db.Model(&models.Session{}).Where("user_id = ?", owner.ID).Update("revoked_at", time.Now()).Error; err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := client.db.Model(&owner).Update("status", models.UserStatusDisabled).Error; err != nil {
					t.Fatal(err)
				}
			case "permission-denied":
				for _, permission := range []string{authz.PermissionJobsReadOwn, authz.PermissionJobsReadAll} {
					if err := client.db.Create(&models.UserAuthorizationRule{UserID: owner.ID, PermissionCode: permission, Effect: models.AuthorizationEffectDeny, CreatedBy: owner.ID}).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			client.queueEvents.Publish(services.JobEvent{Type: "job.updated", JobID: "must-not-leak", JobType: "download", At: time.Now()})
			_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
			for {
				var event services.JobEventEnvelope
				if err := connection.ReadJSON(&event); err != nil {
					break
				}
				if event.Data.JobID == "must-not-leak" {
					t.Fatal("revoked session received a privileged event")
				}
			}
		})
	}
}

func TestQueueWebSocketReleasesHandlerOnSilentClientDisconnect(t *testing.T) {
	client := newTestClient(t)
	client.setup(t)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); client.router.ServeHTTP(w, r) }))
	defer server.Close()
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/jobs/events/ws", http.Header{"Origin": {"http://localhost:3000"}, "Cookie": {client.cookie.String()}})
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnected socket retained its handler/subscription")
	}
}

func TestQueueWebSocketIdleRevocationDoesNotRenewSession(t *testing.T) {
	client := newTestClient(t)
	client.setup(t)
	server := httptest.NewServer(client.router)
	defer server.Close()
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/jobs/events/ws", http.Header{"Origin": {"http://localhost:3000"}, "Cookie": {client.cookie.String()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	var session models.Session
	if err := client.db.First(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := client.db.Model(&session).Update("revoked_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(7 * time.Second))
	_, _, err = connection.ReadMessage()
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("idle revoked session was not closed safely: %v", err)
	}
	var after models.Session
	if err := client.db.First(&after, "id = ?", session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !after.IdleExpiresAt.Equal(session.IdleExpiresAt) || !after.LastSeenAt.Equal(session.LastSeenAt) {
		t.Fatal("background session validation renewed idle activity")
	}
}

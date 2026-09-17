package handlers

import (
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"testing"
	"time"
)

type browserDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *browserDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestBrowserOperationDeadlineUnwrapsGinAndCancels(t *testing.T) {
	writer := &browserDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest("POST", "/", nil)
	cancel := browserOperationDeadline(c)
	if remaining := time.Until(writer.deadline); remaining < 174*time.Second || remaining > 175*time.Second {
		t.Fatal("bounded HTTP deadline not applied through Gin")
	}
	deadline, ok := c.Request.Context().Deadline()
	if !ok || time.Until(deadline) > 170*time.Second {
		t.Fatal("missing bounded request context")
	}
	cancel()
	if c.Request.Context().Err() == nil {
		t.Fatal("request context not cancelled")
	}
}

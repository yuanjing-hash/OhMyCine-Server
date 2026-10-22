package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkConfigurationCannotDiscardInFlightTokenCheckpoint(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(map[bool]string{false: "rename", true: "disable"}[disable], func(t *testing.T) {
			s, actor, connection := createQuarkConnection(t)
			bundle := saveQuarkTestBundle(t, s, connection.ID, func(c *quark.Credentials) { c.TV = testQuarkAuthorization() })
			// Model the exact production lock boundary: a broker has consumed
			// the old refresh token but its replacement has not returned yet.
			entered, release := make(chan struct{}), make(chan struct{})
			checkpointDone := make(chan error, 1)
			go func() {
				checkpointDone <- quark.WithCredentialUpdate(context.Background(), connection.ID, func() error {
					close(entered)
					<-release
					next := bundle
					next.TV = testQuarkAuthorization()
					next.TV.RefreshToken = "consumed-token-replacement"
					next.TV.Generation++
					next.Revision++
					raw, _ := json.Marshal(next)
					return s.quarkCredentialPersister(connection.ID, connection.Revision)(context.Background(), bundle.Revision, raw)
				})
			}()
			<-entered
			name, enabled := "renamed", !disable
			input := UpdateConnectionInput{Name: &name, Revision: connection.Revision}
			if disable {
				input.Enabled = &enabled
			}
			editDone := make(chan error, 1)
			go func() {
				_, err := s.Update(actor, connection.ID, input, RequestContext{})
				editDone <- err
			}()
			var early *error
			select {
			case err := <-editDone:
				early = &err
			case <-time.After(100 * time.Millisecond):
			}
			close(release)
			checkpointErr := <-checkpointDone
			var editErr error
			if early != nil {
				editErr = *early
			} else {
				editErr = <-editDone
			}
			record, saved, readErr := s.quarkCredentials(context.Background(), connection.ID)
			if checkpointErr != nil || editErr != nil || readErr != nil || saved.TV.RefreshToken != "consumed-token-replacement" {
				t.Fatalf("configuration discarded consumed token: checkpoint=%v edit=%v read=%v saved=%t", checkpointErr, editErr, readErr, saved.TV != nil && saved.TV.RefreshToken == "consumed-token-replacement")
			}
			if early != nil || record.Enabled != enabled || record.Revision != connection.Revision+1 {
				t.Fatal("configuration did not serialize after checkpoint or lost its revision/disabled fence")
			}
		})
	}
}

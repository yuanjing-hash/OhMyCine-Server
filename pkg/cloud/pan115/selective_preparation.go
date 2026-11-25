package pan115

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type selectedOwnerPayload struct {
	PlanID string `json:"plan_id"`
	Digest string `json:"digest"`
}

func (c *Client) claimSelectedOwner(ctx context.Context, plan selectedPlan) error {
	id := selectedID(plan)
	resource := offlineResourceKey(plan.InfoHash)
	store := c.selective.store.(cloud.ResourceOperationStore)
	// New claims use a plan-specific identity so retaining a cancelled owner's
	// receipt cannot block the next task with the same hash. Existing legacy
	// claims are discovered by resource, not replaced or silently adopted.
	ownerID := "omc-selected-owner-" + selectedDigest([]string{resource, id})
	owner := selectedReceipt(c, ownerID, "selective_offline_owner", selectedOwnerPayload{id, selectedDigest(plan)})
	existing, err := store.ResourceClaim(ctx, resource)
	if errors.Is(err, cloud.ErrOperationNotFound) {
		if _, exists, readErr := c.selectedNativeTask(ctx, plan.InfoHash, ""); readErr != nil {
			return readErr
		} else if exists {
			return selectedError(cloud.CodeOfflineTaskExists)
		}
		if err = store.Claim(ctx, resource, owner); err != nil {
			existing, err = store.ResourceClaim(ctx, resource)
		} else {
			existing = owner
		}
	}
	if err != nil || existing.Provider != owner.Provider || existing.AccountID != owner.AccountID || existing.Route != owner.Route || existing.Kind != owner.Kind || existing.State != "prepared" || string(existing.Payload) != string(owner.Payload) {
		return selectedError(cloud.CodeOfflineTaskExists)
	}
	return nil
}

// The submit receipt is persisted before native provider I/O. Neither a seed
// receipt nor a directory receipt is evidence that an offline download exists.
func (c *Client) selectedPreparationOnly(ctx context.Context, id string) (bool, error) {
	r, err := c.selective.store.Load(ctx, id+"/submit")
	if errors.Is(err, cloud.ErrOperationNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var stage selectedStage
	if r.Provider != cloud.ProviderPan115 || r.AccountID != c.selective.account || r.Route != "cookie" || r.Kind != "selective_offline_submit" || json.Unmarshal(r.Payload, &stage) != nil {
		return false, selectedError(cloud.CodeResponseInvalid)
	}
	return r.State == "prepared" && stage.DispatchedAt == nil && len(stage.Result) == 0, nil
}

func (c *Client) AbandonSelectedOfflinePreparation(ctx context.Context, request cloud.SelectiveOfflineRequest, deleteData bool) (bool, error) {
	plan, _, err := c.selectedPlanForRequest(request)
	if err != nil {
		return false, err
	}
	id := selectedID(plan)
	saved, err := c.loadSelectedPlan(ctx, id, request.Authority)
	if errors.Is(err, cloud.ErrOperationNotFound) {
		// Without a plan this executor could not create any remote resource.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if selectedDigest(saved) != selectedDigest(plan) {
		return false, selectedError(cloud.CodeResponseInvalid)
	}
	prepared, err := c.selectedPreparationOnly(ctx, id)
	if err != nil || !prepared || !deleteData {
		return prepared, err
	}
	// Only the frozen task directory may be removed; no new directory, seed or
	// native task is created to make cancellation succeed.
	if _, err = c.selectedRoot(ctx, plan.ParentID, plan.StorageRootID); err != nil {
		return false, err
	}
	workspace, found, err := c.selectedDirectory(ctx, plan.ParentID, plan.Tag)
	if err != nil || !found {
		return err == nil, err
	}
	r, err := c.selective.store.Load(ctx, id+"/directory")
	if err != nil {
		return false, selectedError(cloud.CodeResponseInvalid)
	}
	var stage selectedStage
	input, _ := json.Marshal([]string{selectedDigest(plan), plan.ParentID, plan.Tag})
	if r.Provider != cloud.ProviderPan115 || r.AccountID != c.selective.account || r.Route != "cookie" || r.Kind != "selective_offline_directory" || json.Unmarshal(r.Payload, &stage) != nil || string(stage.Input) != string(input) {
		return false, selectedError(cloud.CodeResponseInvalid)
	}
	if r.State == "confirmed" {
		var original cloud.Item
		if json.Unmarshal(stage.Result, &original) != nil || original.ID != workspace.ID || original.ParentID != workspace.ParentID || original.Name != workspace.Name || !original.IsDir {
			return false, selectedError(cloud.CodeResponseInvalid)
		}
	} else if r.State != "unknown" && r.State != "submitting" {
		return false, selectedError(cloud.CodeResponseInvalid)
	}
	page, err := c.List(ctx, workspace.ID, cloud.PageRequest{Limit: 2})
	if err != nil {
		return false, err
	}
	if page.HasMore || len(page.Items) > 1 {
		return false, selectedError(cloud.CodeResponseInvalid)
	}
	for _, item := range page.Items {
		if item.ParentID != workspace.ID || item.ID == "" || item.IsDir || item.Name != "omc-seed-"+plan.MetainfoSHA1+".torrent" || item.Size != plan.MetainfoSize || !strings.EqualFold(item.SHA1, plan.MetainfoSHA1) {
			return false, selectedError(cloud.CodeResponseInvalid)
		}
	}
	if err = c.Recycle(ctx, workspace.ID); err != nil {
		return false, err
	}
	return true, nil
}

var _ cloud.SelectiveOfflinePreparationController = (*Client)(nil)

func (c *Client) ReleaseSelectedOfflinePreparation(ctx context.Context, request cloud.SelectiveOfflineRequest) error {
	plan, _, err := c.selectedPlanForRequest(request)
	if err != nil {
		return err
	}
	id := selectedID(plan)
	r, err := c.selective.store.Load(ctx, id)
	if errors.Is(err, cloud.ErrOperationNotFound) {
		// Claim precedes plan persistence. Retire that same missing plan too,
		// so a failed plan Create cannot leave an orphaned hash reservation.
		r = selectedReceipt(c, id, "selective_offline_plan", plan)
		if err = c.selective.store.Create(ctx, r); err != nil {
			r, err = c.selective.store.Load(ctx, id)
		}
	}
	if err != nil {
		return err
	}
	saved, err := c.loadSelectedPlan(ctx, id, request.Authority)
	if err != nil || selectedDigest(saved) != selectedDigest(plan) {
		if err != nil {
			return err
		}
		return selectedError(cloud.CodeResponseInvalid)
	}
	prepared, err := c.selectedPreparationOnly(ctx, id)
	if err != nil || !prepared {
		if err != nil {
			return err
		}
		return selectedUnknown()
	}
	// Retire the old plan first. A crash between these writes leaves a retryable
	// cleanup, never a released owner whose old worker can submit again.
	if r.State != "abandoned" {
		retired := r
		retired.State, retired.Revision, retired.UpdatedAt = "abandoned", r.Revision+1, time.Now().UTC()
		if err := c.selective.store.Update(ctx, r.Revision, retired); err != nil {
			return err
		}
	}
	store := c.selective.store.(cloud.ResourceOperationStore)
	resource := offlineResourceKey(plan.InfoHash)
	owner, err := store.ResourceClaim(ctx, resource)
	if errors.Is(err, cloud.ErrOperationNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var authority selectedOwnerPayload
	if owner.Provider != cloud.ProviderPan115 || owner.AccountID != c.selective.account || owner.Route != "cookie" {
		return selectedError(cloud.CodeResponseInvalid)
	}
	if owner.Kind != "selective_offline_owner" {
		return nil
	}
	if json.Unmarshal(owner.Payload, &authority) != nil {
		return selectedError(cloud.CodeResponseInvalid)
	}
	if authority.PlanID != id {
		// Another task may have acquired this resource after an earlier cleanup
		// succeeded but its acknowledgement was lost. Leave that owner intact.
		return nil
	}
	if authority.Digest != selectedDigest(plan) || owner.State != "prepared" {
		return selectedError(cloud.CodeResponseInvalid)
	}
	next := owner
	next.State, next.Revision, next.UpdatedAt = "failed", owner.Revision+1, time.Now().UTC()
	return store.ReleaseResourceClaim(ctx, resource, owner.Revision, next)
}

var _ cloud.SelectiveOfflinePreparationReleaser = (*Client)(nil)

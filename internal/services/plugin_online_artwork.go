package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

const maxOnlineArtworkReferences = 512

// projectOnlineArtwork is the last Server boundary before provider media JSON
// reaches Player. It removes raw image URLs when the active plugin package
// cannot turn them into a scoped Server reference.
func (s *PluginRepositoryService) projectOnlineArtwork(ctx context.Context, pluginID, connectionID string, raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		return nil, appError(CodePluginResponseInvalid, "在线媒体图片响应无效", err)
	}
	count := 0
	if err := s.walkOnlineArtwork(ctx, pluginID, connectionID, value, 0, &count); err != nil {
		return nil, err
	}
	projected, err := json.Marshal(value)
	if err != nil {
		return nil, appError(CodePluginResponseInvalid, "在线媒体图片响应无效", err)
	}
	return projected, nil
}

func (s *PluginRepositoryService) projectOnlineArtworkForLibrary(ctx context.Context, libraryID string, raw json.RawMessage) (json.RawMessage, error) {
	_, connection, _, err := s.onlineLibrary(libraryID)
	if err != nil {
		return nil, err
	}
	return s.projectOnlineArtwork(ctx, connection.PluginID, connection.ID, raw)
}

func (s *PluginRepositoryService) walkOnlineArtwork(ctx context.Context, pluginID, connectionID string, value any, depth int, count *int) error {
	if depth > 20 {
		return appError(CodePluginResponseInvalid, "在线媒体图片响应层级无效", nil)
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if onlineArtworkKey(key) {
				if rawURL, ok := child.(string); ok {
					if ref := s.registerOnlineArtwork(ctx, pluginID, connectionID, rawURL, count); ref != "" {
						typed[key] = ref
					} else {
						delete(typed, key)
					}
				} else if values, ok := child.([]any); ok && onlineArtworkArrayKey(key) {
					images := make([]any, 0, len(values))
					for _, item := range values {
						rawURL, ok := item.(string)
						if !ok {
							continue
						}
						if ref := s.registerOnlineArtwork(ctx, pluginID, connectionID, rawURL, count); ref != "" {
							images = append(images, ref)
						}
					}
					typed[key] = images
				} else {
					delete(typed, key)
				}
				continue
			}
			if err := s.walkOnlineArtwork(ctx, pluginID, connectionID, child, depth+1, count); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := s.walkOnlineArtwork(ctx, pluginID, connectionID, child, depth+1, count); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *PluginRepositoryService) registerOnlineArtwork(ctx context.Context, pluginID, connectionID, upstream string, count *int) string {
	if upstream == "" || *count >= maxOnlineArtworkReferences || s.artwork == nil {
		return ""
	}
	*count++
	ref, err := s.artwork.RegisterArtwork(ctx, pluginID, connectionID, upstream)
	if err != nil || ref == "" {
		return ""
	}
	return "/api/v1/player/artwork/" + ref
}

func onlineArtworkKey(key string) bool {
	switch key {
	case "posterUrl", "backdropUrl", "artworkUrl", "profileUrl", "avatarUrl", "coverUrl", "thumbnailUrl", "imageUrl", "logoUrl",
		"stillUrl", "poster_url", "backdrop_url", "artwork_url", "profile_url", "avatar_url", "cover_url", "thumbnail_url", "image_url", "logo_url", "still_url",
		"stillUrls", "backdropUrls", "still_urls", "backdrop_urls":
		return true
	default:
		return false
	}
}

func onlineArtworkArrayKey(key string) bool {
	switch key {
	case "stillUrls", "backdropUrls", "still_urls", "backdrop_urls":
		return true
	default:
		return false
	}
}

package legacy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/model"
	pubsnapshot "github.com/TyrEamon/xrw-album/publisher/internal/snapshot"
)

func ExportSnapshot(ctx context.Context, store *Store, outDir string, limit int, options pubsnapshot.Options) (string, int, int, error) {
	if err := options.Validate(); err != nil {
		return "", 0, 0, err
	}
	if options.ImageBase == "" || options.SigningSecret == "" {
		return "", 0, 0, fmt.Errorf("legacy snapshot requires GIMG_PUBLIC_BASE and GIMG_SIGNING_SECRET")
	}
	candidates, err := store.PendingSnapshots(ctx, limit)
	if err != nil || len(candidates) == 0 {
		return "", 0, 0, err
	}

	batch := pubsnapshot.Batch{Version: 1, ExportedAt: time.Now().UTC().Format(time.RFC3339)}
	for _, candidate := range candidates {
		if candidate.Status == "invalid" {
			batch.RemovedIDs = append(batch.RemovedIDs, candidate.AlbumID)
			continue
		}
		album, images, err := store.SnapshotAlbum(ctx, candidate.AlbumID)
		if err != nil {
			return "", 0, 0, fmt.Errorf("legacy snapshot album %s: %w", candidate.AlbumID, err)
		}
		if len(images) == 0 || len(images) != album.Expected {
			return "", 0, 0, fmt.Errorf("legacy snapshot album %s expects %d images but has %d", album.ID, album.Expected, len(images))
		}
		source := album.Source
		if source == "" {
			source = SourceLinuxDO85W
		}
		payload := model.PublishPayload{
			ID: album.ID, Source: source, Title: album.Title, Count: len(images),
			Href: "/album/" + album.ID, Status: "ok", Photos: make([]model.Image, len(images)),
		}
		for index, image := range images {
			if image.Status != "uploaded" || image.TGFileID == "" || image.Width < 1 || image.Height < 1 {
				return "", 0, 0, fmt.Errorf("legacy snapshot album %s image %d is incomplete", album.ID, image.SortOrder)
			}
			imageURL, err := pubsnapshot.SignedTelegramURL(options.ImageBase, options.SigningSecret, image.TGFileID)
			if err != nil {
				return "", 0, 0, fmt.Errorf("legacy snapshot album %s image %d: %w", album.ID, image.SortOrder, err)
			}
			payload.Photos[index] = model.Image{SortOrder: image.SortOrder, Width: image.Width, Height: image.Height, TGURL: imageURL}
		}
		payload.Cover = payload.Photos[0].TGURL
		batch.Galleries = append(batch.Galleries, payload)
	}

	encoded, err := json.Marshal(batch)
	if err != nil {
		return "", 0, 0, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", 0, 0, err
	}
	now := time.Now().UTC()
	name := fmt.Sprintf("legacy-snapshot-%s-%06d-%06d.json", now.Format("20060102T150405.000000000Z"), candidates[0].Ordinal, candidates[len(candidates)-1].Ordinal)
	target := filepath.Join(outDir, name)
	temporary, err := os.CreateTemp(outDir, ".legacy-snapshot-*.tmp")
	if err != nil {
		return "", 0, 0, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return "", 0, 0, err
	}
	if err := temporary.Close(); err != nil {
		return "", 0, 0, err
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return "", 0, 0, err
	}
	if err := store.MarkSnapshotsExported(ctx, candidates); err != nil {
		return "", 0, 0, err
	}
	return target, len(batch.Galleries), len(batch.RemovedIDs), nil
}

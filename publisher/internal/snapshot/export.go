package snapshot

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/model"
	"github.com/TyrEamon/xrw-album/publisher/internal/store"
)

type Batch struct {
	Version    int                    `json:"version"`
	ExportedAt string                 `json:"exported_at"`
	Galleries  []model.PublishPayload `json:"galleries"`
	RemovedIDs []string               `json:"removed_ids,omitempty"`
}

type Options struct {
	ImageBase     string
	SigningSecret string
}

func Export(ctx context.Context, database *store.Store, outDir string, limit int, options Options) (string, int, error) {
	if err := options.Validate(); err != nil {
		return "", 0, err
	}
	candidates, err := database.PendingSnapshots(ctx, limit)
	if err != nil || len(candidates) == 0 {
		return "", 0, err
	}

	payloads := make([]model.PublishPayload, 0, len(candidates))
	for _, candidate := range candidates {
		payload, err := database.Payload(ctx, candidate.GalleryID, candidate.ChannelID)
		if err != nil {
			return "", 0, fmt.Errorf("snapshot gallery %d: %w", candidate.GalleryID, err)
		}
		if options.ImageBase != "" {
			if len(payload.Photos) != len(payload.TGFiles) {
				return "", 0, fmt.Errorf("snapshot gallery %d: photo and Telegram mapping counts differ", candidate.GalleryID)
			}
			for index := range payload.Photos {
				imageURL, err := SignedTelegramURL(options.ImageBase, options.SigningSecret, payload.TGFiles[index].FileID)
				if err != nil {
					return "", 0, fmt.Errorf("snapshot gallery %d image %d: %w", candidate.GalleryID, payload.Photos[index].SourceImageID, err)
				}
				payload.Photos[index].TGURL = imageURL
			}
			if len(payload.Photos) > 0 {
				payload.Cover = payload.Photos[0].TGURL
			}
		}
		payload.TGFiles = nil
		payloads = append(payloads, payload)
	}

	now := time.Now().UTC()
	batch := Batch{Version: 1, ExportedAt: now.Format(time.RFC3339), Galleries: payloads}
	encoded, err := json.Marshal(batch)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", 0, err
	}
	name := fmt.Sprintf("snapshot-%s-%d-%d.json", now.Format("20060102T150405.000000000Z"), candidates[0].GalleryID, candidates[len(candidates)-1].GalleryID)
	target := filepath.Join(outDir, name)
	temporary, err := os.CreateTemp(outDir, ".snapshot-*.tmp")
	if err != nil {
		return "", 0, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return "", 0, err
	}
	if err := temporary.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return "", 0, err
	}
	if err := database.MarkSnapshotsExported(ctx, candidates); err != nil {
		return "", 0, err
	}
	return target, len(payloads), nil
}

func (options Options) Validate() error {
	if options.ImageBase == "" && options.SigningSecret == "" {
		return nil
	}
	if options.ImageBase == "" || options.SigningSecret == "" {
		return fmt.Errorf("GIMG_PUBLIC_BASE and GIMG_SIGNING_SECRET must be configured together")
	}
	if len(options.SigningSecret) < 32 {
		return fmt.Errorf("GIMG_SIGNING_SECRET must be at least 32 characters")
	}
	parsed, err := url.Parse(options.ImageBase)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("GIMG_PUBLIC_BASE must be an HTTPS origin without query or fragment")
	}
	return nil
}

func SignedTelegramURL(imageBase, secret, fileID string) (string, error) {
	if strings.TrimSpace(fileID) == "" {
		return "", fmt.Errorf("Telegram file_id is empty")
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(fileID))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return strings.TrimRight(imageBase, "/") + "/tg/" + payload + "." + signature, nil
}

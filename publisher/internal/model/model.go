package model

import "fmt"

type GallerySummary struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	Category       string `json:"category"`
	ImageCount     int    `json:"image_count"`
	UploadedImages int    `json:"uploaded_images"`
	Status         string `json:"status"`
	UpdatedAt      string `json:"updated_at"`
	CoverImageID   int64  `json:"-"`
}

func (g GallerySummary) AlbumID() string {
	return fmt.Sprintf("veil-%d", g.ID)
}

type Gallery struct {
	SourceGalleryID int64
	AlbumID         string
	Title           string
	Category        string
	ImageCount      int
	CoverImageID    int64
	SourceUpdatedAt string
	Tags            []string
	TargetChatID    string
	Status          string
	LastError       string
}

type Image struct {
	SourceImageID int64  `json:"source_image_id"`
	GalleryID     int64  `json:"-"`
	SortOrder     int    `json:"id"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	TGURL         string `json:"url"`
	TGFileID      string `json:"-"`
	TGFileUnique  string `json:"-"`
	TGMessageID   int64  `json:"-"`
	TGPublicKey   string `json:"-"`
	TGMimeType    string `json:"-"`
	Status        string `json:"-"`
	RetryCount    int    `json:"-"`
}

type PublishPayload struct {
	ID              string   `json:"id"`
	Source          string   `json:"source"`
	SourceGalleryID int64    `json:"source_gallery_id"`
	SourceUpdatedAt string   `json:"source_updated_at,omitempty"`
	Title           string   `json:"title"`
	Category        string   `json:"category,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	Count           int      `json:"count"`
	Cover           string   `json:"cover"`
	Href            string   `json:"href"`
	Status          string   `json:"status"`
	Photos          []Image  `json:"photos"`
	TGFiles         []TGFile `json:"tg_files,omitempty"`
}

type TGFile struct {
	PublicKey   string `json:"public_key"`
	URL         string `json:"url"`
	FileID      string `json:"file_id"`
	FileUnique  string `json:"file_unique_id,omitempty"`
	MessageID   int64  `json:"message_id,omitempty"`
	ChannelID   string `json:"channel_id"`
	ContentType string `json:"content_type,omitempty"`
}

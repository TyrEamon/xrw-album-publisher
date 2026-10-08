package localupload

import "strings"

// Features names the optional parts of the uploader a build can turn off. The
// Android shell turns off the Telegram Web userscript import: the script only
// runs inside a desktop browser extension, and a phone has nowhere to put it.
type Features struct {
	// TelegramImportDisabled hides the userscript's pairing code, stops serving
	// the script itself and refuses its ingest endpoints. The import drafts stay,
	// because the gallery-site browser lands in the same list.
	TelegramImportDisabled bool
}

// TelegramImport reports whether this build carries the userscript import.
func (f Features) TelegramImport() bool {
	return !f.TelegramImportDisabled
}

// ParseFeatures reads the LOCAL_UPLOADER_DISABLE list, for example
// "telegram-import". Unknown names are ignored, so a name added by a newer build
// cannot keep an older one from starting.
func ParseFeatures(value string) Features {
	var features Features
	for _, name := range strings.Split(value, ",") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "telegram-import":
			features.TelegramImportDisabled = true
		}
	}
	return features
}

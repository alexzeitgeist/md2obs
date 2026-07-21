package watcher

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const notificationSuffix = ".watch-notify"

// NotificationPath is the private cross-process wake-up file associated with
// one state database. SQLite remains authoritative for watch membership; the
// notification contents are deliberately opaque.
func NotificationPath(databasePath string) string {
	return databasePath + notificationSuffix
}

// NotifyImport wakes watch processes after imports may have changed their
// vault-scoped membership. Callers may coalesce multiple imports into one
// notification.
func NotifyImport(databasePath string) error {
	path := NotificationPath(databasePath)
	token := strconv.FormatInt(time.Now().UnixNano(), 10) + "\n"
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("write watch notification %s: %w", path, err)
	}
	return nil
}

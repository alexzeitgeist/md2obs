package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// ReadAndHash reads the whole file (Markdown files are small) and returns
// its content with the hex SHA-256. Reading once keeps the hashed bytes and
// the written bytes identical even if the file changes concurrently.
func ReadAndHash(path string) (content []byte, sha256Hex string, err error) {
	content, err = os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	return content, HashBytes(content), nil
}

// HashBytes returns the hex SHA-256 of content.
func HashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

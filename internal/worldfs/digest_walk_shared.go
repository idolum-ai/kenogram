package worldfs

import (
	"fmt"
	"os"
)

const maxDigestDirectoryDepth = 256

type digestWalkState struct {
	limits        DigestLimits
	entries       []DigestEntry
	metadataBytes int64
	fileBytes     int64
}

func digestSpecialType(mode os.FileMode) string {
	switch {
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeNamedPipe != 0:
		return "fifo"
	case mode&os.ModeDevice != 0:
		return "device"
	default:
		return "special"
	}
}

func (s *digestWalkState) appendMetadata(entry DigestEntry) error {
	if err := validateDigestEntryUTF8(entry); err != nil {
		return err
	}
	if s.limits.MaxEntries > 0 && len(s.entries) >= s.limits.MaxEntries {
		return fmt.Errorf("workspace observation exceeds %d entries", s.limits.MaxEntries)
	}
	bytes := int64(len(entry.Path) + len(entry.Link))
	if s.limits.MaxMetadataBytes > 0 && bytes > s.limits.MaxMetadataBytes-s.metadataBytes {
		return fmt.Errorf("workspace observation exceeds %d metadata bytes", s.limits.MaxMetadataBytes)
	}
	s.metadataBytes += bytes
	s.entries = append(s.entries, entry)
	return nil
}

func (s *digestWalkState) addFileBytes(size int64) error {
	if size < 0 {
		return fmt.Errorf("workspace regular file has negative size")
	}
	if s.limits.MaxFileBytes > 0 && size > s.limits.MaxFileBytes-s.fileBytes {
		return fmt.Errorf("workspace observation exceeds %d regular-file bytes", s.limits.MaxFileBytes)
	}
	s.fileBytes += size
	return nil
}

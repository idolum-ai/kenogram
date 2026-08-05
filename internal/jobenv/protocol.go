// Package jobenv defines the bounded, binary stdin handoff used by the direct
// job adapter to establish the target's exact environment without placing
// secret bytes in provider argv or the provider process environment.
package jobenv

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	MaximumItems      = 128
	MaximumValueBytes = 16 << 10
	maximumNameBytes  = 255
	magic             = "KENOGRAM_JOB_ENV_V1\x00"
)

type Item struct {
	Name  string
	Value []byte
}

func Encode(items []Item) ([]byte, error) {
	if len(items) > MaximumItems {
		return nil, errors.New("target environment exceeds item bound")
	}
	var out bytes.Buffer
	out.WriteString(magic)
	if err := binary.Write(&out, binary.BigEndian, uint16(len(items))); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, item := range items {
		if err := validate(item); err != nil {
			return nil, err
		}
		if _, exists := seen[item.Name]; exists {
			return nil, fmt.Errorf("duplicate target environment name %q", item.Name)
		}
		seen[item.Name] = struct{}{}
		if err := binary.Write(&out, binary.BigEndian, uint16(len(item.Name))); err != nil {
			return nil, err
		}
		if err := binary.Write(&out, binary.BigEndian, uint32(len(item.Value))); err != nil {
			return nil, err
		}
		out.WriteString(item.Name)
		out.Write(item.Value)
	}
	return out.Bytes(), nil
}

func Decode(reader io.Reader) ([]Item, error) {
	limited := &io.LimitedReader{R: reader, N: maximumDocumentBytes() + 1}
	header := make([]byte, len(magic))
	if _, err := io.ReadFull(limited, header); err != nil || string(header) != magic {
		return nil, errors.New("target environment header is invalid")
	}
	var count uint16
	if err := binary.Read(limited, binary.BigEndian, &count); err != nil || count > MaximumItems {
		return nil, errors.New("target environment item count is invalid")
	}
	items := make([]Item, 0, int(count))
	seen := map[string]struct{}{}
	for index := 0; index < int(count); index++ {
		var nameSize uint16
		var valueSize uint32
		if err := binary.Read(limited, binary.BigEndian, &nameSize); err != nil {
			return nil, fmt.Errorf("read target environment name size: %w", err)
		}
		if err := binary.Read(limited, binary.BigEndian, &valueSize); err != nil {
			return nil, fmt.Errorf("read target environment value size: %w", err)
		}
		if nameSize == 0 || nameSize > maximumNameBytes || valueSize > MaximumValueBytes {
			return nil, errors.New("target environment item exceeds bounds")
		}
		name, value := make([]byte, nameSize), make([]byte, valueSize)
		if _, err := io.ReadFull(limited, name); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(limited, value); err != nil {
			return nil, err
		}
		item := Item{Name: string(name), Value: value}
		if err := validate(item); err != nil {
			return nil, err
		}
		if _, exists := seen[item.Name]; exists {
			return nil, fmt.Errorf("duplicate target environment name %q", item.Name)
		}
		seen[item.Name] = struct{}{}
		items = append(items, item)
	}
	var trailing [1]byte
	if n, err := limited.Read(trailing[:]); n != 0 || !errors.Is(err, io.EOF) {
		return nil, errors.New("target environment contains trailing bytes")
	}
	return items, nil
}

func validate(item Item) error {
	if len(item.Name) == 0 || len(item.Name) > maximumNameBytes || strings.ContainsAny(item.Name, "=\x00") {
		return errors.New("target environment name is invalid")
	}
	if len(item.Value) > MaximumValueBytes || bytes.IndexByte(item.Value, 0) >= 0 {
		return errors.New("target environment value is oversized or contains NUL")
	}
	return nil
}

func maximumDocumentBytes() int64 {
	return int64(len(magic)+2) + MaximumItems*int64(2+4+maximumNameBytes+MaximumValueBytes)
}

package jobcontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

func ParseRequest(raw []byte) (Request, error) {
	var value Request
	if err := decode(raw, MaximumRequestBytes, &value); err != nil {
		return value, fmt.Errorf("decode job request: %w", err)
	}
	if err := ValidateRequest(value); err != nil {
		return value, err
	}
	return value, nil
}

func ParseResult(raw []byte) (Result, error) {
	var value Result
	if err := decode(raw, MaximumResultBytes, &value); err != nil {
		return value, fmt.Errorf("decode job result: %w", err)
	}
	if err := ValidateResult(value); err != nil {
		return value, err
	}
	return value, nil
}

func ParseManifest(raw []byte) (Manifest, error) {
	var value Manifest
	if err := decode(raw, MaximumManifestBytes, &value); err != nil {
		return value, fmt.Errorf("decode job evidence manifest: %w", err)
	}
	if err := ValidateManifest(value); err != nil {
		return value, err
	}
	return value, nil
}

func ParseProvenance(raw []byte) (Provenance, error) {
	var value Provenance
	if err := decode(raw, MaximumProvenanceBytes, &value); err != nil {
		return value, fmt.Errorf("decode executable provenance: %w", err)
	}
	if err := ValidateProvenance(value); err != nil {
		return value, err
	}
	return value, nil
}

func ParseRuntimeObservation(raw []byte) (RuntimeObservation, error) {
	var value RuntimeObservation
	if err := decode(raw, MaximumManifestBytes, &value); err != nil {
		return value, fmt.Errorf("decode runtime observation: %w", err)
	}
	if err := ValidateRuntimeObservation(value); err != nil {
		return value, err
	}
	return value, nil
}

func ParseEgressEvidence(raw []byte) (EgressEvidence, error) {
	var value EgressEvidence
	if err := decode(raw, MaximumEgressEvidenceBytes, &value); err != nil {
		return value, fmt.Errorf("decode egress evidence: %w", err)
	}
	if err := ValidateEgressEvidence(value); err != nil {
		return value, err
	}
	return value, nil
}

// ValidateJSONDocument applies the common bounded, UTF-8, duplicate-key, and
// trailing-data rules to an otherwise provider-owned JSON observation.
func ValidateJSONDocument(raw []byte, maximum int) error {
	var value any
	return decode(raw, maximum, &value)
}

func decode(raw []byte, maximum int, target any) error {
	if len(raw) == 0 {
		return errors.New("document is empty")
	}
	if len(raw) > maximum {
		return fmt.Errorf("document exceeds %d bytes", maximum)
	}
	if !utf8.Valid(raw) {
		return errors.New("document is not valid UTF-8")
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("document contains trailing JSON")
		}
		return fmt.Errorf("decode trailing data: %w", err)
	}
	return nil
}

func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("duplicate object key %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("unexpected closing JSON delimiter")
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("document contains trailing JSON")
		}
		return err
	}
	return nil
}

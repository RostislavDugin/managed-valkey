package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/gin-gonic/gin"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
)

const maxJSONBodySize = 16 * 1024

func decodeJSON(c *gin.Context, destination any) error {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxJSONBodySize+1))
	if err != nil {
		return invalidJSON("invalid_json")
	}
	if len(body) > maxJSONBodySize {
		return invalidJSON("body_too_large")
	}
	if err := validateJSONKeys(body); err != nil {
		return invalidJSON("invalid_json")
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return invalidJSON("invalid_json")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalidJSON("invalid_json")
	}

	return nil
}

func validateJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := readJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("после JSON есть лишние данные")
	}

	return nil
}

func readJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delimiter {
	case '{':
		keys := map[string]struct{}{}
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("ключ JSON не является строкой")
			}
			if _, exists := keys[key]; exists {
				return errors.New("повторяющийся ключ JSON")
			}
			keys[key] = struct{}{}
			if valueErr := readJSONValue(decoder); valueErr != nil {
				return valueErr
			}
		}
	case '[':
		for decoder.More() {
			if valueErr := readJSONValue(decoder); valueErr != nil {
				return valueErr
			}
		}
	default:
		return errors.New("неожиданный разделитель JSON")
	}

	_, err = decoder.Token()

	return err
}

func invalidJSON(reason string) error {
	return apierr.New(
		apierr.CodeValidationFailed,
		"Проверьте формат запроса",
		map[string]any{"reason": reason},
	)
}

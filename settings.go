package main

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/doug-martin/goqu/v9"
	"github.com/labstack/echo/v4"

	"subbed/internal/subtitle"
)

const (
	settingTranscribeModel = "transcribe_model"
	settingTranslateModel  = "translate_model"
	maxModelLen            = 200
)

// GetSetting returns a stored setting; the bool is false when it was never set.
func (r *Repository) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	found, err := r.db.From("settings").Select("value").Where(goqu.C("key").Eq(key)).ScanValContext(ctx, &value)
	if err != nil {
		return "", false, fmt.Errorf("failed to read setting %s: %w", key, err)
	}
	return value, found, nil
}

// SetSetting stores a setting, replacing its previous value.
func (r *Repository) SetSetting(ctx context.Context, key, value string) error {
	_, err := r.db.Insert("settings").
		Rows(goqu.Record{"key": key, "value": value}).
		OnConflict(goqu.DoUpdate("key", goqu.Record{"value": goqu.L("excluded.value")})).
		Executor().
		ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to save setting %s: %w", key, err)
	}
	return nil
}

// DeleteSetting removes a setting, so its default applies again.
func (r *Repository) DeleteSetting(ctx context.Context, key string) error {
	_, err := r.db.Delete("settings").Where(goqu.C("key").Eq(key)).Executor().ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to delete setting %s: %w", key, err)
	}
	return nil
}

// transcribeModel is the speech-to-text model to use: the one the admin chose, else the default.
func (r *Repository) transcribeModel(ctx context.Context) (string, error) {
	model, found, err := r.GetSetting(ctx, settingTranscribeModel)
	if err != nil || !found || model == "" {
		return subtitle.DefaultTranscribeModel, err
	}
	return model, nil
}

type settingsResponse struct {
	// TranscribeModel is what the admin set; empty means the default is used
	TranscribeModel        string `json:"transcribe_model"`
	TranscribeModelDefault string `json:"transcribe_model_default"`
	TranslateModel         string `json:"translate_model"`
	TranslateModelDefault  string `json:"translate_model_default"`
}

// translateModel uses the saved choice, then the built-in default.
func (r *Repository) translateModel(ctx context.Context) (string, error) {
	model, _, err := r.GetSetting(ctx, settingTranslateModel)
	return cmp.Or(model, subtitle.DefaultTranslateModel), err
}

func readSettings(ctx context.Context, repo *Repository) (settingsResponse, error) {
	model, _, err := repo.GetSetting(ctx, settingTranscribeModel)
	if err != nil {
		return settingsResponse{}, err
	}
	translateModel, _, err := repo.GetSetting(ctx, settingTranslateModel)
	return settingsResponse{
		TranscribeModel: model, TranscribeModelDefault: subtitle.DefaultTranscribeModel,
		TranslateModel: translateModel, TranslateModelDefault: subtitle.DefaultTranslateModel,
	}, err
}

func getSettings(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		settings, err := readSettings(c.Request().Context(), repo)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, settings)
	}
}

// updateSettings changes the settings that are present in the request. An empty model restores the default.
func updateSettings(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req struct {
			TranscribeModel *string `json:"transcribe_model"`
			TranslateModel  *string `json:"translate_model"`
		}
		if err := c.Bind(&req); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid request")
		}
		ctx := c.Request().Context()

		models := []struct {
			key   string
			value *string
		}{
			{settingTranscribeModel, req.TranscribeModel},
			{settingTranslateModel, req.TranslateModel},
		}
		for _, setting := range models {
			if setting.value == nil {
				continue
			}
			model := strings.TrimSpace(*setting.value)
			if utf8.RuneCountInString(model) > maxModelLen || strings.ContainsFunc(model, func(r rune) bool { return r == ' ' || r < ' ' }) {
				return echo.NewHTTPError(http.StatusBadRequest, "Invalid model name")
			}
		}
		for _, setting := range models {
			if setting.value == nil {
				continue
			}
			model := strings.TrimSpace(*setting.value)
			var err error
			if model == "" {
				err = repo.DeleteSetting(ctx, setting.key)
			} else {
				err = repo.SetSetting(ctx, setting.key, model)
			}
			if err != nil {
				return err
			}
		}

		settings, err := readSettings(ctx, repo)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, settings)
	}
}

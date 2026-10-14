package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

const openRouterModels = "https://openrouter.ai/api/v1/models"

// modelsTTL is how long OpenRouter's model list is kept.
const modelsTTL = time.Hour

// Model is one OpenRouter model. Prices are USD per million tokens, null
// when OpenRouter gives none or a variable one.
type Model struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Modality    string   `json:"modality,omitempty"`
	Context     int64    `json:"context_length"`
	MaxOutput   int64    `json:"max_completion_tokens,omitempty"`
	PriceIn     *float64 `json:"price_in"`
	PriceOut    *float64 `json:"price_out"`
	Tools       bool     `json:"tools"`
}

type modelList struct {
	url    string
	client *http.Client

	mu      sync.Mutex
	fetched time.Time
	list    []Model
}

// get returns the cached list, fetching it when older than modelsTTL. When
// the fetch fails, an older list is better than none.
func (m *modelList) get(ctx context.Context) ([]Model, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.list != nil && time.Since(m.fetched) < modelsTTL {
		return m.list, nil
	}
	list, err := m.fetch(ctx)
	if err != nil {
		if m.list != nil {
			return m.list, nil
		}
		return nil, err
	}
	m.list, m.fetched = list, time.Now()
	return list, nil
}

func (m *modelList) fetch(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", m.url, resp.Status)
	}
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Context     int64  `json:"context_length"`
			Arch        struct {
				Modality string `json:"modality"`
			} `json:"architecture"`
			TopProvider struct {
				MaxCompletion int64 `json:"max_completion_tokens"`
			} `json:"top_provider"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			Params []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s: %w", m.url, err)
	}
	list := make([]Model, 0, len(body.Data))
	for _, d := range body.Data {
		desc := d.Description
		if len(desc) > 360 {
			desc = desc[:360] + "…"
		}
		list = append(list, Model{
			ID: d.ID, Name: d.Name, Description: desc,
			Modality:  d.Arch.Modality,
			Context:   d.Context,
			MaxOutput: d.TopProvider.MaxCompletion,
			PriceIn:   perMillion(d.Pricing.Prompt), PriceOut: perMillion(d.Pricing.Completion),
			Tools: slices.Contains(d.Params, "tools"),
		})
	}
	return list, nil
}

// perMillion converts OpenRouter's price per token; it marks a variable
// price (openrouter/auto) as -1.
func perMillion(s string) *float64 {
	p, err := strconv.ParseFloat(s, 64)
	if err != nil || p < 0 {
		return nil
	}
	// Round off the float noise of the conversion: 0.2, not 0.19999999999999998.
	p = math.Round(p*1e12) / 1e6
	return &p
}

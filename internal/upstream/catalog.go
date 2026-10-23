package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
)

// ParseModelsDev only accepts explicitly zero-priced models belonging to the
// OpenCode provider. Free on another provider does not imply free on OpenCode.
func ParseModelsDev(data []byte, provider string) ([]Model, error) {
	var catalog map[string]struct {
		Models map[string]struct {
			ID   string `json:"id"`
			Cost *struct {
				Input  *float64 `json:"input"`
				Output *float64 `json:"output"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, err
	}
	p, ok := catalog[provider]
	if !ok {
		return nil, fmt.Errorf("models.dev provider %q not found", provider)
	}
	result := []Model{}
	for key, m := range p.Models {
		if m.Cost == nil || m.Cost.Input == nil || m.Cost.Output == nil || *m.Cost.Input != 0 || *m.Cost.Output != 0 {
			continue
		}
		id := m.ID
		if id == "" {
			id = key
		}
		result = append(result, Model{ID: id, Object: "model", OwnedBy: provider})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	if len(result) == 0 {
		return nil, fmt.Errorf("no explicitly free models in provider %s", provider)
	}
	return result, nil
}
func (c *Client) modelsDev(ctx context.Context, opts CallOptions) ([]Model, error) {
	sel, err := c.Egress.Select(c.CatalogURL, opts.Proxy, opts.Circuit)
	if err != nil {
		return nil, err
	}
	transport, err := c.Egress.Transport(sel)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.CatalogURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("models.dev returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	provider := c.CatalogProvider
	if provider == "" {
		provider = "opencode"
	}
	return ParseModelsDev(data, provider)
}

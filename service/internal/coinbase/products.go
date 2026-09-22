package coinbase

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Products is GET /products, as received: every product Coinbase lists (838 on 2026-09-21).
func Products(ctx context.Context, userAgent string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, restURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := restClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coinbase: HTTP %d for products", resp.StatusCode)
	}
	return body, nil
}

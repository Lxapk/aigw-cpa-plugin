package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// postBillingWithBody POSTs any []byte body to a billing endpoint, returning
// the response payload and HTTP status.
func (c *workBuddyClient) postBillingWithBody(ctx context.Context, creds *workBuddyCredentials, variant wbVariant, path string, body []byte) ([]byte, int, error) {
	if creds == nil || creds.AccessToken == "" {
		return nil, 0, errNoToken
	}
	endpoint := variant.apiBase() + path
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return nil, 0, errRequest
	}
	applyWorkBuddyHeadersVariant(req.Header, creds, variant)
	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, 0, fmt.Errorf("请求失败: %w", errDo)
	}
	defer resp.Body.Close()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取响应失败: %w", errRead)
	}
	return raw, resp.StatusCode, nil
}

var errNoToken = fmt.Errorf("缺少访问令牌")

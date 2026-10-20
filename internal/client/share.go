package client

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// Shared spaces (msg/share.go). Every write goes to the nodes in order, the first that answers
// taking it: a version and its chunks must reach the same node, so a caller that fails over
// uploads the chunks again (msg.RefusedShareMissing says so).

// ShareSync returns the share records the answering node holds beyond vv (node to node).
func (c *Client) ShareSync(vv map[string]uint64) ([]msg.ShareRecord, error) {
	b, _ := json.Marshal(vv)
	var recs []msg.ShareRecord
	err := c.do(c.URLs, http.MethodGet, "/v1/share/sync?vv="+url.QueryEscape(string(b)), nil, &recs)
	return recs, err
}

// ShareChunk fetches one sealed chunk of thread.
func (c *Client) ShareChunk(thread, sha string) ([]byte, error) {
	raw := &Raw{Limit: msg.ShareChunkMax}
	err := c.do(c.URLs, http.MethodGet, "/v1/share/chunk?thread="+url.QueryEscape(thread)+"&sha="+url.QueryEscape(sha), nil, raw)
	return raw.Data, err
}

// ShareUpload stores one sealed chunk for thread and returns its hash.
func (c *Client) ShareUpload(thread string, sealed []byte) (string, error) {
	var out msg.ShareChunkDone
	err := c.do(c.URLs, http.MethodPost, "/v1/share/upload?thread="+url.QueryEscape(thread), msg.ShareChunkPut{Data: sealed}, &out)
	return out.SHA256, err
}

// SharePut asks the node to take a version; the record says the number it was given.
func (c *Client) SharePut(thread string, v msg.ShareVersion) (msg.ShareRecord, error) {
	var out msg.ShareRecord
	err := c.do(c.URLs, http.MethodPost, "/v1/share/put", msg.SharePut{Thread: thread, Ver: v}, &out)
	return out, err
}

// ShareKey asks the node to keep a sealed key.
func (c *Client) ShareKey(thread string, k msg.ShareKey) (msg.ShareRecord, error) {
	var out msg.ShareRecord
	err := c.do(c.URLs, http.MethodPost, "/v1/share/key", msg.ShareKeyPut{Thread: thread, Key: k}, &out)
	return out, err
}

// ShareIndex is what the answering node holds of thread's space. A node from before shared spaces
// answers 404 (IsNotFound).
func (c *Client) ShareIndex(thread string) (msg.ShareIndex, error) {
	var out msg.ShareIndex
	err := c.do(c.URLs, http.MethodGet, "/v1/share/index?thread="+url.QueryEscape(thread), nil, &out)
	return out, err
}

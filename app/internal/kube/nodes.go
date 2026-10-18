package kube

import (
	"context"
	"net/url"
)

// Node is the metadata needed for KubeWarm targeting. Labels come from the
// API server, never from a worker's self-reported payload.
type Node struct {
	Metadata struct {
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

func (n Node) Ready() bool {
	if n.Metadata.DeletionTimestamp != nil || n.Spec.Unschedulable {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	var all []Node
	cont := ""
	for {
		q := url.Values{"limit": {"500"}}
		if cont != "" {
			q.Set("continue", cont)
		}
		var page struct {
			Items    []Node `json:"items"`
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
		}
		if err := c.get(ctx, "/api/v1/nodes?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		cont = page.Metadata.Continue
		if cont == "" {
			return all, nil
		}
	}
}

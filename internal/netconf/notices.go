package netconf

import (
	"errors"
	"fmt"
	"time"
)

const (
	NoticeInfo     = "info"
	NoticeWarning  = "warning"
	NoticeCritical = "critical"
)

// Notice is operator context shown on the Forks page, such as a cancellation and the call that
// decided it. It is plain text: the web renders it without HTML, and links only as http(s) anchors.
type Notice struct {
	Network  string       `json:"network,omitempty" yaml:"network,omitempty"`
	Fork     string       `json:"fork,omitempty" yaml:"fork,omitempty"`
	Severity string       `json:"severity" yaml:"severity"`
	Title    string       `json:"title" yaml:"title"`
	Body     string       `json:"body,omitempty" yaml:"body,omitempty"`
	Links    []NoticeLink `json:"links,omitempty" yaml:"links,omitempty"`
	Starts   *time.Time   `json:"starts,omitempty" yaml:"starts,omitempty"`
	Expires  *time.Time   `json:"expires,omitempty" yaml:"expires,omitempty"`
}

type NoticeLink struct {
	Label string `json:"label" yaml:"label"`
	URL   string `json:"url" yaml:"url"`
}

const (
	maxNoticeTitle = 200
	maxNoticeBody  = 4000
	maxNoticeLinks = 8
)

func ValidateNotices(list []Notice) error {
	var errs []error
	for i, n := range list {
		at := fmt.Sprintf("notice %d", i+1)
		switch {
		case n.Title == "" || len(n.Title) > maxNoticeTitle:
			errs = append(errs, fmt.Errorf("%s: title is required and at most %d bytes", at, maxNoticeTitle))
		case len(n.Body) > maxNoticeBody:
			errs = append(errs, fmt.Errorf("%s: body is over %d bytes", at, maxNoticeBody))
		case n.Severity != NoticeInfo && n.Severity != NoticeWarning && n.Severity != NoticeCritical:
			errs = append(errs, fmt.Errorf("%s: severity %q is not info, warning or critical", at, n.Severity))
		case n.Network != "" && !knownNetwork(n.Network):
			errs = append(errs, fmt.Errorf("%s: unknown network %q", at, n.Network))
		case n.Starts != nil && n.Expires != nil && !n.Starts.Before(*n.Expires):
			errs = append(errs, fmt.Errorf("%s: starts is not before expires", at))
		case len(n.Links) > maxNoticeLinks:
			errs = append(errs, fmt.Errorf("%s: more than %d links", at, maxNoticeLinks))
		}
		for _, l := range n.Links {
			if l.Label == "" || !httpURL(l.URL) {
				errs = append(errs, fmt.Errorf("%s: link %q needs a label and an http(s) URL", at, l.URL))
			}
		}
	}
	return errors.Join(errs...)
}

func NoticesAt(network string, at time.Time) []Notice {
	releasesMu.RLock()
	defer releasesMu.RUnlock()
	out := []Notice{}
	for _, n := range notices {
		if (n.Network == "" || n.Network == network) &&
			(n.Starts == nil || !at.Before(*n.Starts)) && (n.Expires == nil || at.Before(*n.Expires)) {
			out = append(out, n)
		}
	}
	return out
}

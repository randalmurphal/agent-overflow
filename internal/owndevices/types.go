// Package owndevices carries the bounded, public membership catalog. Session
// credentials and private keys never belong in this replicated shape.
package owndevices

import (
	"encoding/base64"
	"errors"
	"reflect"
	"sort"
	"strings"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/entityid"
)

const Capability = "own-devices.v1"
const MaxMembers = 128

type Member struct {
	DeviceClass   string                `json:"deviceClass,omitempty"`
	KeyThumbprint string                `json:"keyThumbprint"`
	BackendID     string                `json:"backendId,omitempty"`
	Name          string                `json:"name"`
	Routes        []computerroute.Route `json:"routes"`
	Generation    int64                 `json:"generation"`
	Removed       bool                  `json:"removed"`
}
type List struct {
	ConnectedBackendIDs []string `json:"connectedBackendIds"`
	ExcludedBackendIDs  []string `json:"excludedBackendIds"`
	Enabled             bool     `json:"enabled"`
	SelfKeyThumbprint   string   `json:"selfKeyThumbprint"`
	Members             []Member `json:"members"`
}

func ValidKey(key string) bool {
	b, e := base64.RawURLEncoding.DecodeString(key)
	return e == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == key
}
func Validate(m Member) error {
	if (m.DeviceClass != "" && m.DeviceClass != "desktop" && m.DeviceClass != "phone" && m.DeviceClass != "browser" && m.DeviceClass != "cli") || !ValidKey(m.KeyThumbprint) || m.Generation < 1 || m.Generation > 1<<52 || len(m.Name) > 256 || strings.TrimSpace(m.Name) != m.Name || (m.BackendID != "" && !entityid.Valid(m.BackendID)) || len(m.Routes) > computerroute.MaxRoutes {
		return errors.New("invalid own-device member")
	}
	if m.BackendID == "" && len(m.Routes) > 0 {
		return errors.New("a device without a backend cannot advertise routes")
	}
	for _, r := range m.Routes {
		n, e := computerroute.Normalize(r)
		if e != nil || n != r {
			return errors.New("invalid own-device route")
		}
	}
	return nil
}

// Merge combines membership generations, with removal winning concurrent writes.
// A sponsor may introduce an unknown address, but never replace established
// endpoint trust. Existing metadata is updated only by direct self-registration.
func Merge(current, incoming []Member) ([]Member, bool, error) {
	if len(current) > MaxMembers || len(incoming) > MaxMembers {
		return nil, false, errors.New("too many own devices")
	}
	rows := make(map[string]Member, len(current)+len(incoming))
	for _, m := range current {
		if e := Validate(m); e != nil {
			return nil, false, e
		}
		rows[m.KeyThumbprint] = m
	}
	for _, m := range incoming {
		if e := Validate(m); e != nil {
			return nil, false, e
		}
		if old, ok := rows[m.KeyThumbprint]; ok {
			next := old
			if m.Generation > old.Generation || (m.Generation == old.Generation && m.Removed) {
				next.Generation = m.Generation
				next.Removed = m.Removed
			}
			if next.BackendID == "" && !next.Removed {
				next.BackendID = m.BackendID
				next.Routes = m.Routes
				next.Name = m.Name
			}
			rows[m.KeyThumbprint] = next
		} else {
			rows[m.KeyThumbprint] = m
		}
	}
	if len(rows) > MaxMembers {
		return nil, false, errors.New("too many own devices")
	}
	out := make([]Member, 0, len(rows))
	ids := map[string]string{}
	for _, m := range rows {
		if m.BackendID != "" && !m.Removed {
			if other, ok := ids[m.BackendID]; ok && other != m.KeyThumbprint {
				return nil, false, errors.New("a backend cannot belong to two device identities")
			}
			ids[m.BackendID] = m.KeyThumbprint
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].KeyThumbprint < out[j].KeyThumbprint })
	return out, !reflect.DeepEqual(out, current), nil
}

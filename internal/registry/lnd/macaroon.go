package lnd

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Permission is one thing a macaroon allows: an entity and an action.
type Permission struct {
	Entity string
	Action string
}

func (p Permission) String() string { return p.Entity + ":" + p.Action }

// Credential is a macaroon, and what it turned out to allow.
type Credential struct {
	// Hex is what goes in the request header. Never logged, never put in an
	// error, never persisted anywhere but the file it came from.
	Hex string
	// Permissions is what the macaroon grants.
	Permissions []Permission
	// Readable is false when the permission list could not be established.
	//
	// The caller must not read that as "no write permissions found". A decoder
	// that quietly returned an empty list would turn "we could not tell" into
	// "it is safe", which is the same false assurance the distinct-node check
	// exists to prevent.
	Readable bool
}

// writeActions are the actions that let a credential change something. Forktower
// only ever reads, so any of these is more authority than it needs.
var writeActions = map[string]bool{
	"write":    true,
	"generate": true,
	"macaroon": true,
}

// Overprivileged reports whether this credential can change anything.
//
// Not a reason to refuse to start: both target platforms hand out admin
// macaroons, and refusing would mean no protection at all. It is a reason to say
// so loudly — a daemon holding more authority than it needs is one whose
// compromise costs more than it should.
func (c Credential) Overprivileged() bool {
	for _, p := range c.Permissions {
		if writeActions[strings.ToLower(p.Action)] {
			return true
		}
	}
	return false
}

// Grants reports whether the macaroon allows a specific thing.
func (c Credential) Grants(entity, action string) bool {
	for _, p := range c.Permissions {
		if strings.EqualFold(p.Entity, entity) && strings.EqualFold(p.Action, action) {
			return true
		}
	}
	return false
}

// LoadMacaroon reads a macaroon file and inspects what it allows.
//
// Inspected by decoding it, never by calling an endpoint to see what works: the
// endpoints that would answer that question are the ones that change things, and
// a probe that mutates is not a probe.
func LoadMacaroon(path string) (Credential, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // an operator-supplied credential path, by design
	if err != nil {
		return Credential{}, fmt.Errorf("reading the Lightning credential: %w", err)
	}
	if len(raw) == 0 {
		return Credential{}, errors.New("the Lightning credential file is empty")
	}

	cred := Credential{Hex: hex.EncodeToString(raw)}
	perms, ok := DecodePermissions(raw)
	cred.Permissions, cred.Readable = perms, ok
	return cred, nil
}

// Macaroon v2 binary field types, from libmacaroons' own format.
const (
	fieldEOS        = 0
	fieldLocation   = 1
	fieldIdentifier = 2
	fieldSignature  = 6
)

// DecodePermissions reads the entity/action pairs out of a macaroon.
//
// Two nested formats, both small enough to read directly rather than take a
// dependency for:
//
//   - the macaroon itself is libmacaroons' v2 binary: a version byte, then
//     length-prefixed fields, of which the identifier is the one that matters;
//   - the identifier is macaroon-bakery's own protobuf, whose field 3 repeats an
//     Op of {entity, actions...}.
//
// Returns false when it cannot establish the list, which callers must treat as
// unknown rather than as safe.
func DecodePermissions(raw []byte) ([]Permission, bool) {
	id, ok := identifierOf(raw)
	if !ok {
		return nil, false
	}
	return opsFrom(id)
}

// identifierOf pulls the identifier field out of a v2 binary macaroon.
func identifierOf(raw []byte) ([]byte, bool) {
	if len(raw) < 2 || raw[0] != 2 {
		// Not the v2 binary format. Older macaroons exist; rather than guess at
		// them, say so and let the caller report the check as unavailable.
		return nil, false
	}
	p := raw[1:]

	for len(p) > 0 {
		fieldType, n := binary.Uvarint(p)
		if n <= 0 {
			return nil, false
		}
		p = p[n:]

		if fieldType == fieldEOS {
			// End of this section. The first section holds the macaroon's own
			// location and identifier, which is all this needs.
			return nil, false
		}

		length, n := binary.Uvarint(p)
		if n <= 0 || uint64(len(p[n:])) < length {
			return nil, false
		}
		p = p[n:]
		data := p[:length]
		p = p[length:]

		switch fieldType {
		case fieldIdentifier:
			return data, true
		case fieldLocation, fieldSignature:
			// Not needed. The signature in particular is the part that carries
			// the macaroon's authority, and nothing here looks at it.
		}
	}
	return nil, false
}

// opsFrom reads the bakery identifier's protobuf.
//
// A minimal reader rather than a protobuf dependency: three field numbers, two
// wire types, and no need to be general. Anything it does not understand it
// skips, and anything malformed makes the whole read fail rather than return
// half a list — half a permission list read as complete is exactly the wrong
// answer for a security check.
func opsFrom(id []byte) ([]Permission, bool) {
	if len(id) == 0 {
		return nil, false
	}

	// The bakery prefixes its marshalled identifier with a version byte, and the
	// version in the field is not one this could have guessed: a real LND
	// read-only macaroon carries 3, where the format's own history would suggest
	// 1 or 2.
	//
	// So it is skipped by what it cannot be rather than by what it is. The first
	// protobuf field here is field 1, wire type 2, whose tag byte is 0x0a — any
	// leading byte below that cannot begin this message and is therefore the
	// version. That holds for whatever the next version turns out to be.
	if id[0] < 0x0a {
		id = id[1:]
	}

	var perms []Permission
	for len(id) > 0 {
		tag, n := binary.Uvarint(id)
		if n <= 0 {
			return nil, false
		}
		id = id[n:]

		fieldNum, wireType := tag>>3, tag&7
		switch wireType {
		case 2: // length-delimited
			length, n := binary.Uvarint(id)
			if n <= 0 || uint64(len(id[n:])) < length {
				return nil, false
			}
			id = id[n:]
			data := id[:length]
			id = id[length:]

			if fieldNum == 3 { // repeated Op
				op, ok := opFrom(data)
				if !ok {
					return nil, false
				}
				perms = append(perms, op...)
			}

		case 0: // varint
			_, n := binary.Uvarint(id)
			if n <= 0 {
				return nil, false
			}
			id = id[n:]

		default:
			// A wire type this does not read. Rather than skip an unknown length
			// and risk misreading everything after it, stop.
			return nil, false
		}
	}
	return perms, len(perms) > 0
}

// opFrom reads one Op: entity in field 1, actions repeated in field 2.
func opFrom(data []byte) ([]Permission, bool) {
	var (
		entity  string
		actions []string
	)
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 {
			return nil, false
		}
		data = data[n:]

		fieldNum, wireType := tag>>3, tag&7
		if wireType != 2 {
			return nil, false
		}
		length, n := binary.Uvarint(data)
		if n <= 0 || uint64(len(data[n:])) < length {
			return nil, false
		}
		data = data[n:]
		value := string(data[:length])
		data = data[length:]

		switch fieldNum {
		case 1:
			entity = value
		case 2:
			actions = append(actions, value)
		}
	}

	if entity == "" || len(actions) == 0 {
		return nil, false
	}
	out := make([]Permission, 0, len(actions))
	for _, a := range actions {
		out = append(out, Permission{Entity: entity, Action: a})
	}
	return out, true
}

package app

import (
	"encoding/json"
	"fmt"

	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/web"
)

// The strategy builder's engine half, handed to the web layer as two functions so that web does
// not import engine (internal/layers): buildVersion turns the page's shape into a validated,
// labelled version, presetsJSON lists the standard shapes.

func buildVersion(shape []byte) (web.Built, error) {
	var s engine.Shape
	if err := json.Unmarshal(shape, &s); err != nil {
		return web.Built{}, web.BuildRefused{Why: "The shape could not be read: " + err.Error()}
	}
	p, err := engine.FromShape(s)
	if err != nil {
		return web.Built{}, web.BuildRefused{Why: err.Error()}
	}
	params, err := json.Marshal(p)
	if err != nil {
		return web.Built{}, fmt.Errorf("params: %w", err)
	}
	parent := "Value"
	if p.Exit == "ev" {
		parent = "Scalper"
	}
	return web.Built{Name: p.Name, Blurb: p.Blurb, Params: params, Parent: parent, Control: s.Control}, nil
}

var presetsJSON = func() func() []byte {
	b, err := json.Marshal(engine.Presets())
	if err != nil {
		panic(err) // a constant list that failed to marshal: a programming error, found at start
	}
	return func() []byte { return b }
}()

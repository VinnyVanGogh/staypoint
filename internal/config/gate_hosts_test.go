package config

import (
	"reflect"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestGateHostsFromTOML(t *testing.T) {
	var c Config
	src := "[gates]\nship_review = true\n\n[gates.hosts]\ndev = [\"mansol-dev\"]\nprod = [\"mansol-prod\", \"10.0.0.5\"]\ndev_services = [\"mansol_apps\"]\n"
	if err := toml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	want := GateHostsConfig{Dev: []string{"mansol-dev"}, Prod: []string{"mansol-prod", "10.0.0.5"}, DevServices: []string{"mansol_apps"}}
	if !reflect.DeepEqual(c.Gates.Hosts, want) {
		t.Fatalf("hosts = %+v, want %+v", c.Gates.Hosts, want)
	}
	if !c.Gates.ShipReviewEnabled() {
		t.Fatal("ship_review lost next to [gates.hosts]")
	}
}

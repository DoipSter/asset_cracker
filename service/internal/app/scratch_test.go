package app

import "testing"

func TestRefuseScratchDatabase(t *testing.T) {
	if err := refuseScratchDatabase("assetcracker_dev"); err == nil {
		t.Fatal("a scratch database would have been recorded into")
	}
	if err := refuseScratchDatabase("assetcracker"); err != nil {
		t.Fatal(err)
	}
	if err := refuseScratchDatabase(""); err != nil {
		t.Fatal(err)
	}
}

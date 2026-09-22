package config

import (
	"reflect"
	"testing"
)

func TestCatalogueSettings(t *testing.T) {
	t.Setenv("AC_CATALOGUE_CATEGORIES", " Crypto, Financials ,,")
	t.Setenv("AC_MAX_SELECTED", "4")
	c := Load()
	if !reflect.DeepEqual(c.CatalogueCategories, []string{"Crypto", "Financials"}) || c.MaxSelected != 4 {
		t.Errorf("categories %v, max %d", c.CatalogueCategories, c.MaxSelected)
	}
	for _, bad := range []string{"", "0", "-3", "ten"} {
		t.Setenv("AC_MAX_SELECTED", bad)
		t.Setenv("AC_CATALOGUE_CATEGORIES", "")
		if c := Load(); c.MaxSelected != 0 || c.CatalogueCategories != nil {
			t.Errorf("AC_MAX_SELECTED=%q: max %d, categories %v; want 0 and nil, so the catalogue's defaults apply", bad, c.MaxSelected, c.CatalogueCategories)
		}
	}
}

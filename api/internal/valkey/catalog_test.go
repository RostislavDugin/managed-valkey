package valkey_test

import (
	"errors"
	"testing"

	"github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

func Test_CreateCatalog_WithResourceLimits_BuildsAllowedSizeGrid(t *testing.T) {
	catalog := testCatalog(t, 4, 16)
	want := []valkey.Size{
		{VCPU: 1, RAMGB: 1},
		{VCPU: 1, RAMGB: 2},
		{VCPU: 1, RAMGB: 4},
		{VCPU: 2, RAMGB: 8},
		{VCPU: 4, RAMGB: 16},
	}
	if len(catalog.Items) != len(want) {
		t.Fatalf("размер сетки %d, ожидался %d: %+v", len(catalog.Items), len(want), catalog.Items)
	}
	for index := range want {
		if catalog.Items[index] != want[index] {
			t.Fatalf("элемент %d равен %+v, ожидался %+v", index, catalog.Items[index], want[index])
		}
	}
}

func Test_CreateCatalog_WithoutResourceLimits_ReturnsInvalidSizeError(t *testing.T) {
	_, err := valkey.NewCatalog(valkey.CatalogConfig{})
	if !errors.Is(err, valkey.ErrInvalidSize) {
		t.Fatalf("ошибка %v, ожидалась %v", err, valkey.ErrInvalidSize)
	}
}

func Test_CalculatePriceAndReserve_WithSingleAndHaModes_ReturnsExpectedResourcesAndRates(t *testing.T) {
	catalog := testCatalog(t, 4, 16)
	size := valkey.Size{VCPU: 1, RAMGB: 2}

	single, err := catalog.PriceCoinsPerHour("single", size)
	if err != nil || single != 225 {
		t.Fatalf("цена single %d, ошибка %v", single, err)
	}
	ha, err := catalog.PriceCoinsPerHour("ha", size)
	if err != nil || ha != 675 {
		t.Fatalf("цена ha %d, ошибка %v", ha, err)
	}

	reserved, err := valkey.Reserve("ha", valkey.Size{VCPU: 1, RAMGB: 4}, valkey.Size{})
	if err != nil || reserved != (valkey.Size{VCPU: 3, RAMGB: 12}) {
		t.Fatalf("резерв %+v, ошибка %v", reserved, err)
	}
}

func Test_CalculateMonthlyPrice_WithCatalogSizes_ReturnsExpectedAmounts(t *testing.T) {
	catalog := testCatalog(t, 4, 16)
	tests := []struct {
		size   valkey.Size
		single int64
		ha     int64
	}{
		{size: valkey.Size{VCPU: 1, RAMGB: 1}, single: 126000, ha: 378000},
		{size: valkey.Size{VCPU: 1, RAMGB: 2}, single: 162000, ha: 486000},
		{size: valkey.Size{VCPU: 1, RAMGB: 4}, single: 234000, ha: 702000},
		{size: valkey.Size{VCPU: 2, RAMGB: 8}, single: 468000, ha: 1404000},
		{size: valkey.Size{VCPU: 4, RAMGB: 16}, single: 936000, ha: 2808000},
	}

	for _, testCase := range tests {
		single, err := catalog.PriceCoinsPerHour("single", testCase.size)
		if err != nil || single*valkey.HoursPerMonth != testCase.single {
			t.Errorf("single %+v: цена %d, ошибка %v", testCase.size, single, err)
		}
		ha, err := catalog.PriceCoinsPerHour("ha", testCase.size)
		if err != nil || ha*valkey.HoursPerMonth != testCase.ha {
			t.Errorf("ha %+v: цена %d, ошибка %v", testCase.size, ha, err)
		}
	}
}

func testCatalog(t *testing.T, maxVCPU, maxRAMGB int) valkey.Catalog {
	t.Helper()

	catalog, err := valkey.NewCatalog(valkey.CatalogConfig{
		MaxVCPU:           maxVCPU,
		MaxRAMGB:          maxRAMGB,
		VCPUCoinsPerHour:  125,
		RAMGBCoinsPerHour: 50,
		Domain:            "valkey.localhost",
		Port:              41379,
	})
	if err != nil {
		t.Fatalf("создать каталог: %v", err)
	}

	return catalog
}

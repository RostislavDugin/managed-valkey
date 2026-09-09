package valkey

import (
	"errors"
	"fmt"
	"slices"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

const HoursPerMonth = 720

var ErrInvalidSize = errors.New("недопустимый размер Valkey")

type Size struct {
	VCPU  int `json:"vcpu"`
	RAMGB int `json:"ram_gb"`
}

var availableSizes = [...]Size{
	{VCPU: 1, RAMGB: 1},
	{VCPU: 1, RAMGB: 2},
	{VCPU: 1, RAMGB: 4},
	{VCPU: 2, RAMGB: 8},
	{VCPU: 4, RAMGB: 16},
}

type Pricing struct {
	VCPUCoinsPerHour  int64 `json:"vcpu_coins_per_hour"`
	RAMGBCoinsPerHour int64 `json:"ram_gb_coins_per_hour"`
	HoursPerMonth     int   `json:"hours_per_month"`
}

type Connection struct {
	Domain string `json:"domain"`
	Port   int    `json:"port"`
}

type Catalog struct {
	Items      []Size     `json:"items"`
	Pricing    Pricing    `json:"pricing"`
	Connection Connection `json:"connection"`
}

type CatalogConfig struct {
	MaxVCPU           int
	MaxRAMGB          int
	VCPUCoinsPerHour  int64
	RAMGBCoinsPerHour int64
	Domain            string
	Port              int
}

func NewCatalog(cfg CatalogConfig) (Catalog, error) {
	items := make([]Size, 0, len(availableSizes))
	for _, size := range availableSizes {
		if size.VCPU > cfg.MaxVCPU || size.RAMGB > cfg.MaxRAMGB {
			continue
		}

		items = append(items, size)
	}
	if len(items) == 0 {
		return Catalog{}, fmt.Errorf("%w: сетка пуста", ErrInvalidSize)
	}

	return Catalog{
		Items: items,
		Pricing: Pricing{
			VCPUCoinsPerHour:  cfg.VCPUCoinsPerHour,
			RAMGBCoinsPerHour: cfg.RAMGBCoinsPerHour,
			HoursPerMonth:     HoursPerMonth,
		},
		Connection: Connection{Domain: cfg.Domain, Port: cfg.Port},
	}, nil
}

func (c Catalog) HasSize(size Size) bool {
	return slices.Contains(c.Items, size)
}

func (c Catalog) PriceCoinsPerHour(mode domain.ValkeyInstanceMode, size Size) (int64, error) {
	nodes, err := NodeCount(mode)
	if err != nil {
		return 0, err
	}
	if !c.HasSize(size) {
		return 0, ErrInvalidSize
	}

	perNode := c.Pricing.VCPUCoinsPerHour*int64(size.VCPU) +
		c.Pricing.RAMGBCoinsPerHour*int64(size.RAMGB)

	return perNode * int64(nodes), nil
}

func Reserve(mode domain.ValkeyInstanceMode, desired, applied Size) (Size, error) {
	nodes, err := NodeCount(mode)
	if err != nil {
		return Size{}, err
	}

	return Size{
		VCPU:  nodes * max(desired.VCPU, applied.VCPU),
		RAMGB: nodes * max(desired.RAMGB, applied.RAMGB),
	}, nil
}

func NodeCount(mode domain.ValkeyInstanceMode) (int, error) {
	switch mode {
	case domain.ValkeyInstanceModeSingle:
		return 1, nil
	case domain.ValkeyInstanceModeHA:
		return 3, nil
	default:
		return 0, fmt.Errorf("неизвестный режим %q", mode)
	}
}

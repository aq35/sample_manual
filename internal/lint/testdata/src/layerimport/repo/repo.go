package repo

import (
	"github.com/samber/lo"   // want `samber/lo は DB 境界の層`
	_ "github.com/samber/mo" // want `samber/mo は DB 境界の層`
)

func Use() int { return lo.Sum([]int{1, 2, 3}) }

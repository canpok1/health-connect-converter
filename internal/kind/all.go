package kind

import "sort"

// All は全種別を種別キーの辞書順で返す。
//
// 順序は出力に効く（daily_summary の列順・タブの並び順）ため、辞書順に固定する。
// 種別を足すときはこの一覧に1行加える。
func All() []Kind {
	kinds := []Kind{
		basalMetabolicRateKind{},
		bloodPressureKind{},
		bodyFatKind{},
		distanceKind{},
		exerciseSessionKind{},
		heartRateKind{},
		heartRateVariabilityKind{},
		heightKind{},
		oxygenSaturationKind{},
		respiratoryRateKind{},
		restingHeartRateKind{},
		sleepKind{},
		speedKind{},
		stepsKind{},
		totalCaloriesBurnedKind{},
		weightKind{},
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i].Key() < kinds[j].Key() })
	return kinds
}

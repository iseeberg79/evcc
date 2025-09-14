package metrics

import (
	"fmt"
	"time"

	"github.com/evcc-io/evcc/server/db"
)

type meter struct {
	Meter     int       `json:"meter" gorm:"column:meter;uniqueIndex:meter_ts"`
	Timestamp time.Time `json:"ts" gorm:"column:ts;uniqueIndex:meter_ts"`
	Value     float64   `json:"val" gorm:"column:val"`
}

func Init() error {
	return db.Instance.AutoMigrate(new(meter))
}

// Persist stores 15min consumption in Wh
func Persist(ts time.Time, value float64) error {
	return db.Instance.Create(meter{
		Meter:     1,
		Timestamp: ts.Truncate(15 * time.Minute),
		Value:     value,
	}).Error
}

// SlotNum returns the index of a 15-minute slot in a 24h window
func SlotNum(ts time.Time) int {
	ts = ts.Local()
	return ts.Hour()*4 + ts.Minute()/15
}

// Profile returns a 15min average meter profile in Wh.
// Profile is sorted nach Uhrzeit (%H:%M) unabhängig vom Datum.
// Fehlende Slots werden mit Tagesdurchschnitt aufgefüllt.
func Profile(from time.Time) (*[96]float64, error) {
	db, err := db.Instance.DB()
	if err != nil {
		return nil, err
	}

	// 1. Tagesdurchschnitt berechnen (letzte 24h ab 'from')
	var dayAvg float64
	row := db.QueryRow(`
		SELECT AVG(val)
		FROM meters
		WHERE meter = ? AND ts >= ?
	`, 1, from)
	if err := row.Scan(&dayAvg); err != nil {
		return nil, err
	}

	// 2. Werte abrufen wie im Original
	rows, err := db.Query(`
		SELECT min(ts) AS ts, avg(val) AS val
		FROM meters
		WHERE meter = ? AND ts >= ?
		GROUP BY strftime("%H:%M", ts)
		ORDER BY strftime("%H:%M", ts) ASC
	`, 1, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 3. Slice vorbereiten
	res := make([]float64, 96)
	filled := make([]bool, 96)

	for rows.Next() {
		var tsStr string
		var val float64
		if err := rows.Scan(&tsStr, &val); err != nil {
			return nil, err
		}

    		ts, err := time.Parse("2006-01-02 15:04:05-07:00", tsStr)
    		if err != nil {
        		return nil, fmt.Errorf("failed to parse timestamp '%s': %w", tsStr, err)
    		}

		slot := SlotNum(ts)
		if slot >= 0 && slot < 96 {
			res[slot] = val
			filled[slot] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 4. Fehlende Slots mit Tagesdurchschnitt auffüllen
	for i := 0; i < 96; i++ {
		if !filled[i] {
			res[i] = dayAvg
		}
	}

	//// 5. Werte ausgeben (Debug)
	//total := len(res)
	//for i, val := range res {
	//	hour := i / 4
	//	min := (i % 4) * 15
	//	fmt.Printf("[%d/%d] %02d:%02d -> %.6f\n", i+1, total, hour, min, val)
	//}

	// 6. Direkt als Array-Pointer zurückgeben
	return (*[96]float64)(res[:]), nil
}

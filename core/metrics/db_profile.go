package metrics

import (
	"errors"
	"strconv"
	"time"

	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/tariff"
)

var ErrIncomplete = errors.New("meter profile incomplete")

// profileRow is one time-of-day bucket's aggregated result.
type profileRow struct {
	ts     time.Time
	energy float64
}

// buildProfile assembles a 96-slot 15min profile from time-ordered, one-per-%H:%M rows,
// interpolating a single missing slot (e.g. from a regular restart). Returns
// ErrIncomplete if more than that is missing.
func buildProfile(rows []profileRow) (*[96]float64, error) {
	var prev time.Time
	res := make([]float64, 0, 96)

	for _, r := range rows {
		// interpolate single missing value, maybe due to regular restarts?
		if r.ts.Sub(prev) == 2*tariff.SlotDuration {
			res = append(res, (r.energy+res[len(res)-1])/2)
		}
		prev = r.ts

		res = append(res, r.energy)
	}

	if len(res) != 96 {
		return nil, ErrIncomplete
	}

	return (*[96]float64)(res), nil
}

// energyProfile returns a 15min average meter profile in kWh, pooling every day in the
// window regardless of weekday. Used as the fallback for a weekday whose own profile
// (weekdayProfiles) doesn't have enough dedicated history yet. The profile is sorted by
// timestamp starting at 00:00. It is guaranteed to contain 96 15min values.
func energyProfile(entity entity, from time.Time) (*[96]float64, error) {
	sqlDB, err := db.Instance.DB()
	if err != nil {
		return nil, err
	}

	// COALESCE guards against legacy rows with NULL energy
	rows, err := sqlDB.Query(`SELECT min(ts) AS ts, COALESCE(avg(energy), 0) AS energy
		FROM meters
		WHERE meter = ? AND ts >= ? AND COALESCE(recovered, 0) = 0
		GROUP BY strftime("%H:%M", ts, 'unixepoch', 'localtime')
		ORDER BY strftime("%H:%M", ts, 'unixepoch', 'localtime') ASC`,
		entity.Id, from.Unix(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prof []profileRow
	for rows.Next() {
		var ts SqlTime
		var val float64

		if err := rows.Scan(&ts, &val); err != nil {
			return nil, err
		}

		prof = append(prof, profileRow{time.Time(ts), val})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return buildProfile(prof)
}

// weekdayProfileMinSamples is the minimum number of that weekday's occurrences in the
// window before its own profile is trusted over the pooled fallback. Without this, a
// single occurrence (including any one-off anomaly - a guest, a holiday) would already
// produce a "complete" 96-slot profile and be used with full confidence.
const weekdayProfileMinSamples = 4

// weekdayProfiles returns a 15min average meter profile in kWh for each weekday (index
// 0=Sunday..6=Saturday, matching time.Weekday), queried in a single pass grouped by
// (weekday, time-of-day). Splitting per weekday keeps each day's systematic consumption
// pattern (routine, occupancy) out of the average instead of smearing it across every
// day. A weekday with fewer than weekdayProfileMinSamples occurrences, or an incomplete
// resulting profile, is nil in the result - the caller is expected to fall back to
// energyProfile's pooled profile for that entry.
func weekdayProfiles(entity entity, from time.Time) ([7]*[96]float64, error) {
	var profiles [7]*[96]float64

	sqlDB, err := db.Instance.DB()
	if err != nil {
		return profiles, err
	}

	// strftime('%w', ...) is 0=Sunday..6=Saturday, matching time.Weekday. count(*) is the
	// number of occurrences of that weekday backing this particular time-of-day bucket.
	rows, err := sqlDB.Query(`SELECT strftime('%w', ts, 'unixepoch', 'localtime') AS dow,
			min(ts) AS ts, COALESCE(avg(energy), 0) AS energy, count(*) AS n
		FROM meters
		WHERE meter = ? AND ts >= ? AND COALESCE(recovered, 0) = 0
		GROUP BY dow, strftime("%H:%M", ts, 'unixepoch', 'localtime')
		ORDER BY dow ASC, strftime("%H:%M", ts, 'unixepoch', 'localtime') ASC`,
		entity.Id, from.Unix(),
	)
	if err != nil {
		return profiles, err
	}
	defer rows.Close()

	var byDow [7][]profileRow
	var minSamples [7]int
	for rows.Next() {
		var dow string
		var ts SqlTime
		var val float64
		var n int

		if err := rows.Scan(&dow, &ts, &val, &n); err != nil {
			return profiles, err
		}

		d, err := strconv.Atoi(dow)
		if err != nil {
			return profiles, err
		}

		byDow[d] = append(byDow[d], profileRow{time.Time(ts), val})
		if len(byDow[d]) == 1 || n < minSamples[d] {
			minSamples[d] = n
		}
	}
	if err := rows.Err(); err != nil {
		return profiles, err
	}

	for d := range profiles {
		if minSamples[d] < weekdayProfileMinSamples {
			continue // not enough weeks of this weekday yet
		}

		p, err := buildProfile(byDow[d])
		if err != nil {
			if errors.Is(err, ErrIncomplete) {
				continue // some time-of-day buckets never had data for this weekday
			}
			return profiles, err
		}
		profiles[d] = p
	}

	return profiles, nil
}

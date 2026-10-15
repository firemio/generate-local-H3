package sources

import (
	"encoding/json"
	"errors"
)

// The parts of the JMA JSON the parser reads. overview_forecast/<area>.json is one object;
// forecast/<area>.json is a list of blocks (short-range, then weekly), each with timeSeries.
type jmaOverview struct {
	PublishingOffice string `json:"publishingOffice"`
	ReportDatetime   string `json:"reportDatetime"`
	Text             string `json:"text"`
}

type jmaArea struct {
	Area struct {
		Name string `json:"name"`
	} `json:"area"`
	Weathers     []string `json:"weathers"`
	WeatherCodes []string `json:"weatherCodes"`
	Pops         []string `json:"pops"`
	Temps        []string `json:"temps"`
	TempsMin     []string `json:"tempsMin"`
	TempsMax     []string `json:"tempsMax"`
}

type jmaSeries struct {
	TimeDefines []string  `json:"timeDefines"`
	Areas       []jmaArea `json:"areas"`
}

type jmaBlock struct {
	TimeSeries []jmaSeries `json:"timeSeries"`
}

var errShape = errors.New("jma: forecast without a short-range block")

// parseForecast is aitv/sources.py _parse_forecast on the two raw documents. A document without the
// short-range block (no blocks, no time series, no areas) is an error, so the caller caches "nothing"
// (and keeps a stale forecast on a refresh); any other missing part simply reads as empty.
func parseForecast(overviewRaw, forecastRaw []byte) (*Forecast, error) {
	var overview jmaOverview
	if err := json.Unmarshal(overviewRaw, &overview); err != nil {
		return nil, err
	}
	var forecast []jmaBlock
	if err := json.Unmarshal(forecastRaw, &forecast); err != nil {
		return nil, err
	}
	if len(forecast) == 0 {
		return nil, errShape
	}
	today := forecast[0].TimeSeries
	if len(today) == 0 || len(today[0].Areas) == 0 {
		return nil, errShape
	}
	weather := today[0].Areas[0]
	fc := &Forecast{
		Area:     weather.Area.Name,
		Office:   overview.PublishingOffice,
		Report:   overview.ReportDatetime,
		Overview: cutRunes(strip(collapseWS(overview.Text)), 600),
	}
	for i, t := range today[0].TimeDefines {
		if i >= len(weather.Weathers) {
			break
		}
		fc.Days = append(fc.Days, ForecastDay{Date: cutRunes(t, 10), Weather: collapseWS(weather.Weathers[i])})
	}
	if len(today) > 2 && len(today[2].Areas) > 0 {
		ta := today[2].Areas[0]
		fc.TempPoint = ta.Area.Name
		seen := map[[2]string]bool{}
		// JMA lists (min, max) per day and repeats today's max when the min is already past
		for i, t := range today[2].TimeDefines {
			if i >= len(ta.Temps) {
				break
			}
			v := ta.Temps[i]
			date := cutRunes(t, 10)
			kind := "最低"
			if i%2 == 1 {
				kind = "最高"
			}
			if v != "" && !seen[[2]string{date, v}] {
				seen[[2]string{date, v}] = true
				fc.Temps = append(fc.Temps, date+" "+kind+" "+v+"℃")
			}
		}
	}
	if len(today) > 1 && len(today[1].Areas) > 0 {
		pa := today[1].Areas[0]
		for i, t := range today[1].TimeDefines {
			if i >= len(pa.Pops) {
				break
			}
			if p := pa.Pops[i]; p != "" {
				fc.Pops = append(fc.Pops, sliceRunes(t, 11, 16)+" "+p+"%")
			}
		}
		if len(fc.Pops) > 8 {
			fc.Pops = fc.Pops[:8]
		}
	}
	if len(forecast) > 1 {
		wk := forecast[1].TimeSeries
		var wa, wt jmaArea
		var defines []string
		if len(wk) > 0 {
			defines = wk[0].TimeDefines
			if len(wk[0].Areas) > 0 {
				wa = wk[0].Areas[0]
			}
		}
		if len(wk) > 1 && len(wk[1].Areas) > 0 {
			wt = wk[1].Areas[0]
		}
		for i, t := range defines {
			fc.Week = append(fc.Week, WeekDay{Date: cutRunes(t, 10), Code: at(wa.WeatherCodes, i), Pop: at(wa.Pops, i),
				Min: at(wt.TempsMin, i), Max: at(wt.TempsMax, i)})
		}
	}
	return fc, nil
}

// at is list[i], "" past the end.
func at(list []string, i int) string {
	if i < len(list) {
		return list[i]
	}
	return ""
}

// codeText is _code_text: the word for the first digit of a weather code, "" when unknown or empty.
func codeText(code string) string {
	if code == "" {
		return ""
	}
	return WeatherCodes[cutRunes(code, 1)]
}

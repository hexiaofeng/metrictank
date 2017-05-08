package expr

import (
	"fmt"
	"math"

	"github.com/raintank/metrictank/api/models"
	"gopkg.in/raintank/schema.v1"
)

type FuncPerSecond struct {
	in       []models.Series
	maxValue int64
}

func NewPerSecond() Func {
	return &FuncPerSecond{}
}

func (s *FuncPerSecond) Signature() ([]arg, []arg) {
	return []arg{
			argSeriesList{},
			argInt{"maxValue", true, []validator{IntPositive}, &s.maxValue},
		}, []arg{
			argSeriesList{},
		}
}

func (s *FuncPerSecond) NeedRange(from, to uint32) (uint32, uint32) {
	return from, to
}

func (s *FuncPerSecond) Exec(cache map[Req][]models.Series) ([]interface{}, error) {
	maxValue := math.NaN()
	if s.maxValue > 0 {
		maxValue = float64(s.maxValue)
	}
	var outputs []interface{}
	for _, serie := range s.in {
		out := pointSlicePool.Get().([]schema.Point)
		for i, v := range serie.Datapoints {
			out = append(out, schema.Point{Ts: v.Ts})
			if i == 0 || math.IsNaN(v.Val) || math.IsNaN(serie.Datapoints[i-1].Val) {
				out[i].Val = math.NaN()
				continue
			}
			diff := v.Val - serie.Datapoints[i-1].Val
			if diff >= 0 {
				out[i].Val = diff / float64(serie.Interval)
			} else if !math.IsNaN(maxValue) && maxValue >= v.Val {
				out[i].Val = (maxValue + diff + 1) / float64(serie.Interval)
			} else {
				out[i].Val = math.NaN()
			}
		}
		s := models.Series{
			Target:     fmt.Sprintf("perSecond(%s)", serie.Target),
			Datapoints: out,
			Interval:   serie.Interval,
		}
		outputs = append(outputs, s)
		cache[Req{}] = append(cache[Req{}], s)
	}
	return outputs, nil
}

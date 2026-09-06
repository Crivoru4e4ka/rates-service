package rates

import (
	"fmt"
	"strings"

	"plata-rates/internal/domain"
)

// CrossPrice вычисляет цену пары из курсов, выраженных против некоторой
// базовой валюты (анкора): price(BASE/QUOTE) = R[QUOTE] / R[BASE].
//
// Например, если API вернул курсы «против EUR» (R[EUR]=1), цена USD/MXN
// считается как R[MXN] / R[USD] — кросс-курс. Формула не зависит от того,
// какая база пришла в ответе, поэтому провайдеры устойчивы к тарифам,
// где базовая валюта фиксирована.
func CrossPrice(anchor string, vs map[string]float64, pair domain.Pair) (float64, error) {
	m := make(map[string]float64, len(vs)+1)
	for c, v := range vs {
		m[strings.ToUpper(c)] = v
	}
	if anchor != "" {
		m[strings.ToUpper(anchor)] = 1
	}

	base, okBase := m[pair.Base]
	quote, okQuote := m[pair.Quote]
	if !okBase || !okQuote {
		return 0, fmt.Errorf("в ответе нет курсов для пары %s", pair.String())
	}
	if base <= 0 || quote <= 0 {
		return 0, fmt.Errorf("некорректные курсы в ответе: %s=%v, %s=%v", pair.Base, base, pair.Quote, quote)
	}
	return quote / base, nil
}

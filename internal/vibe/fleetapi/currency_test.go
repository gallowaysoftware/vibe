package fleetapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/prices"
	"github.com/stretchr/testify/require"
)

// The savings screen has two halves in two currencies. Token savings are
// priced out of the vendored table, which is normalized to
// prices.Currency; electricity and capital are the household's own
// money, declared in hosts.yaml. `net = gross − power` and
// `recovered / capital_cost` both cross that seam.
//
// Until `pricing.currency` existed, they crossed it silently: the repo
// owner is Canadian, the hydro bill and the hardware receipts are CAD,
// and the screen subtracted CAD from USD and printed a "$". That is a
// wrong number with no visible symptom — the exact failure mode the rest
// of this screen is built to prevent, on the one term it was not
// checking.

// mixedHosts is savingsHosts with the local currency declared as
// something the price table is not.
func mixedHosts(cur string) *fleetcfg.File {
	h := savingsHosts()
	h.Pricing.Currency = cur
	return h
}

// seedPricedDay puts one measured, priced, powered day in the ledger —
// the shape every assertion below needs.
func seedPricedDay(t *testing.T, s *Server) string {
	t.Helper()
	day := today()
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{
		Req: 100, InFresh: 4_000_000, InCached: 40_000_000, Out: 2_000_000, BusyMS: 3_600_000,
	})
	seedResidency(s, day, "gpu-cell", "", 20*3600)
	return day
}

// TestSavings_UndeclaredCurrencyIsAnAssumptionNotASilentDefault: a fleet
// that has never heard of this field keeps its numbers — changing them
// would be a silent re-statement of history — but the report says out
// loud that it assumed a currency, and the page has a chip for it.
func TestSavings_UndeclaredCurrencyIsAnAssumptionNotASilentDefault(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	seedPricedDay(t, s)
	rep := mustSavings(t, s, WindowAll)

	require.Equal(t, prices.Currency, rep.Currency.Tokens)
	require.Equal(t, prices.Currency, rep.Currency.Local)
	require.False(t, rep.Currency.LocalDeclared,
		"an undeclared currency must be reported as undeclared, not as a fact the config stated")
	require.False(t, rep.Currency.Mixed)
	require.Contains(t, rep.Currency.Note, "pricing.currency",
		"the assumption does not name the field that would settle it")
	// The arithmetic is untouched: the net is still gross − power.
	row := cellRow(t, rep, "gpu-cell")
	require.NotNil(t, row.Net, "an undeclared currency must not blank a net that was correct before")
	require.Equal(t, "net", row.NetLabel)
	require.NotEmpty(t, rep.Payback, "an undeclared currency must not remove the payback bar")
}

// TestSavings_MatchingCurrencyIsSilent: a fleet that declares the price
// table's own currency gets no chip and no note. An honesty affordance
// that fires when nothing is wrong is noise, and noise is what makes the
// real one invisible.
func TestSavings_MatchingCurrencyIsSilent(t *testing.T) {
	s := newSavingsServer(t, mixedHosts(prices.Currency))
	seedPricedDay(t, s)
	rep := mustSavings(t, s, WindowAll)

	require.True(t, rep.Currency.LocalDeclared)
	require.False(t, rep.Currency.Mixed)
	require.Empty(t, rep.Currency.Note)
	require.NotNil(t, cellRow(t, rep, "gpu-cell").Net)
	require.NotEmpty(t, rep.Payback)
}

// TestSavings_MixedCurrencyRefusesToNet is the fix, stated head-on. When
// the two units cannot be reconciled the report does NOT subtract, does
// NOT invent a rate, and does NOT print a plausible dollar figure: the
// net is absent and its label says why — the same answer this screen
// already gives for every other thing it does not know.
func TestSavings_MixedCurrencyRefusesToNet(t *testing.T) {
	s := newSavingsServer(t, mixedHosts("CAD"))
	seedPricedDay(t, s)
	rep := mustSavings(t, s, WindowAll)

	require.True(t, rep.Currency.Mixed)
	require.Equal(t, "CAD", rep.Currency.Local)
	require.Equal(t, prices.Currency, rep.Currency.Tokens)

	row := cellRow(t, rep, "gpu-cell")
	require.NotNil(t, row.Gross, "the gross is a fact about the token side and still renders")
	require.NotNil(t, row.Power, "the electricity is a real cost and still renders, in its own currency")
	require.Nil(t, row.Net,
		"a CAD electricity bill was subtracted from a USD gross; the result is a confident wrong number")
	require.Contains(t, row.NetLabel, "CAD")
	require.Contains(t, row.NetLabel, prices.Currency)
	require.Contains(t, row.NetLabel, "no exchange rate")

	require.Nil(t, rep.Totals.Net, "the fleet total nets across the same seam")
	require.Contains(t, rep.Totals.NetLabel, "no exchange rate")
	require.NotNil(t, rep.Totals.Gross, "refusing the net must not blank the gross")
	require.NotNil(t, rep.Totals.Power, "refusing the net must not blank the power")

	// The note travels IN the payload, for the same reason the caveat
	// does: fleetmcp serves this identical document, and an agent quoting
	// a headline out of it must get the reason with it.
	require.Contains(t, rep.Currency.Note, "will not subtract")
	require.Contains(t, rep.Caveat, "refuses to net them")
}

// TestSavings_MixedCurrencyRemovesThePaybackBarAndSaysWhy: a payback
// percentage is a token-priced numerator over a locally-declared
// denominator. With no rate it is not a rough figure, it is a claim
// about an exchange rate — so there is no bar, on the same principle as
// a cell with no capital_cost at all.
func TestSavings_MixedCurrencyRemovesThePaybackBarAndSaysWhy(t *testing.T) {
	s := newSavingsServer(t, mixedHosts("CAD"))
	seedPricedDay(t, s)
	rep := mustSavings(t, s, WindowAll)

	require.Empty(t, rep.Payback,
		"a payback bar was drawn from a USD numerator over a CAD denominator")
	var found bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "no payback bars") && strings.Contains(n, "gpu-cell") {
			found = true
		}
	}
	require.True(t, found, "the payback strip vanished with no explanation; notes = %v", rep.Notes)

	// And the "electricity looks too cheap" heuristic, which divides the
	// local term by the token term, must not fire across the same seam.
	for _, n := range rep.Notes {
		require.NotContains(t, n, "% of the gross figure",
			"a CAD-over-USD ratio was published as a percentage")
	}
}

// TestSavings_MixedCurrencyIsOnTheWireAndOnThePage traces the refusal
// from the handler to what a reader sees: the currency block is served
// by the real endpoint, and the page labels both halves and refuses to
// print a minus sign between them.
func TestSavings_MixedCurrencyIsOnTheWireAndOnThePage(t *testing.T) {
	s := newSavingsServer(t, mixedHosts("CAD"))
	seedPricedDay(t, s)

	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/fleet/savings?window=all")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var doc map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))

	cur, ok := doc["currency"].(map[string]any)
	require.True(t, ok, "the savings document carries no currency block")
	require.Equal(t, true, cur["mixed"])
	require.Equal(t, "CAD", cur["local"])
	totals, _ := doc["totals"].(map[string]any)
	require.NotContains(t, totals, "net", "a net was published across two currencies")

	page := fleetPageSource(t)
	// Every money figure goes through a formatter that appends the code.
	require.Contains(t, page, `function amt(v, cur) {`, "money on the page carries no currency")
	require.Contains(t, page, "function tok(v) { return amt(v, CUR.tokens); }")
	require.Contains(t, page, "function locAmt(v) { return amt(v, CUR.local); }")
	require.Contains(t, page, "CUR = sv.currency || CUR;",
		"the page never reads the currency block, so its labels are a guess")
	// The hero must not render "gross − power = net" when the two are in
	// different currencies.
	require.Contains(t, page, `sep1.textContent = " − " + locAmt(t.power) + " power = ";`)
	require.Contains(t, page, "NOT subtracted",
		"under a mixed pair the hero still prints a minus sign between two currencies")
	require.Contains(t, page, "MIXED CURRENCY", "there is no chip for a mixed pair")
	require.Contains(t, page, `$("sv-currency")`, "the currency note is not rendered")
}

// TestSavings_CurrencyIsNormalisedBeforeItIsCompared: the comparison
// that decides whether to net is a string equality, so "usd" declared in
// a hand-built config must not read as a different currency from the
// price table's. (The SHAPE of the declared value is validated at load —
// see fleetcfg's TestLoad_PricingCurrency.)
func TestSavings_CurrencyIsNormalisedBeforeItIsCompared(t *testing.T) {
	s := newSavingsServer(t, mixedHosts(strings.ToLower(prices.Currency)))
	seedPricedDay(t, s)
	rep := mustSavings(t, s, WindowAll)
	require.False(t, rep.Currency.Mixed,
		"a lower-cased spelling of the price table's own currency was treated as a different one")
	require.NotNil(t, cellRow(t, rep, "gpu-cell").Net)
}

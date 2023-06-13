package xalign

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/types"
)

const ID = "xalign"

func init() {
	bbgo.RegisterStrategy(ID, &Strategy{})
}

type QuoteCurrencyPreference struct {
	Buy  []string `json:"buy"`
	Sell []string `json:"sell"`
}

type Strategy struct {
	*bbgo.Environment
	Interval                 types.Interval              `json:"interval"`
	PreferredSessions        []string                    `json:"sessions"`
	PreferredQuoteCurrencies *QuoteCurrencyPreference    `json:"quoteCurrencies"`
	ExpectedBalances         map[string]fixedpoint.Value `json:"expectedBalances"`
	UseTakerOrder            bool                        `json:"useTakerOrder"`
	DryRun                   bool                        `json:"dryRun"`

	sessions   map[string]*bbgo.ExchangeSession
	orderBooks map[string]*bbgo.ActiveOrderBook
}

func (s *Strategy) ID() string {
	return ID
}

func (s *Strategy) InstanceID() string {
	var cs []string

	for cur := range s.ExpectedBalances {
		cs = append(cs, cur)
	}

	return ID + strings.Join(s.PreferredSessions, "-") + strings.Join(cs, "-")
}

func (s *Strategy) Subscribe(session *bbgo.ExchangeSession) {
	// session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{Interval: s.Interval})
}

func (s *Strategy) CrossSubscribe(sessions map[string]*bbgo.ExchangeSession) {

}

func (s *Strategy) Validate() error {
	if s.PreferredQuoteCurrencies == nil {
		return errors.New("quoteCurrencies is not defined")
	}

	return nil
}

func (s *Strategy) aggregateBalances(ctx context.Context, sessions map[string]*bbgo.ExchangeSession) (totalBalances types.BalanceMap, sessionBalances map[string]types.BalanceMap) {
	totalBalances = make(types.BalanceMap)
	sessionBalances = make(map[string]types.BalanceMap)

	// iterate the sessions and record them
	for sessionName, session := range sessions {
		// update the account balances and the margin information
		if _, err := session.UpdateAccount(ctx); err != nil {
			log.WithError(err).Errorf("can not update account")
			return
		}

		account := session.GetAccount()
		balances := account.Balances()

		sessionBalances[sessionName] = balances
		totalBalances = totalBalances.Add(balances)
	}

	return totalBalances, sessionBalances
}

func (s *Strategy) selectSessionForCurrency(ctx context.Context, sessions map[string]*bbgo.ExchangeSession, currency string, changeQuantity fixedpoint.Value) (*bbgo.ExchangeSession, *types.SubmitOrder) {
	for _, sessionName := range s.PreferredSessions {
		session := sessions[sessionName]

		var taker = s.UseTakerOrder
		var side types.SideType
		var quoteCurrencies []string
		if changeQuantity.Sign() > 0 {
			quoteCurrencies = s.PreferredQuoteCurrencies.Buy
			side = types.SideTypeBuy
		} else {
			quoteCurrencies = s.PreferredQuoteCurrencies.Sell
			side = types.SideTypeSell
		}

		for _, quoteCurrency := range quoteCurrencies {
			symbol := currency + quoteCurrency
			market, ok := session.Market(symbol)
			if !ok {
				continue
			}

			ticker, err := session.Exchange.QueryTicker(ctx, symbol)
			if err != nil {
				log.WithError(err).Errorf("unable to query ticker on %s", symbol)
				continue
			}

			spread := ticker.Sell.Sub(ticker.Buy)

			// changeQuantity > 0 = buy
			// changeQuantity < 0 = sell
			q := changeQuantity.Abs()

			if q.Compare(market.MinQuantity) < 0 {
				log.Infof("skip dust quantity: %f", q.Float64())
				continue
			}

			log.Infof("%s changeQuantity: %f ticker: %+v market: %+v", symbol, changeQuantity.Float64(), ticker, market)

			switch side {

			case types.SideTypeBuy:
				quoteBalance, ok := session.Account.Balance(quoteCurrency)
				if !ok {
					continue
				}

				price := ticker.Sell
				if taker {
					price = ticker.Sell
				} else if spread.Compare(market.TickSize) > 0 {
					price = ticker.Sell.Sub(market.TickSize)
				} else {
					price = ticker.Buy
				}

				requiredQuoteAmount := q.Mul(price)
				requiredQuoteAmount = requiredQuoteAmount.Round(market.PricePrecision, fixedpoint.Up)
				if requiredQuoteAmount.Compare(quoteBalance.Available) > 0 {
					log.Warnf("required quote amount %f > quote balance %v, skip", requiredQuoteAmount.Float64(), quoteBalance)
					continue
				}

				if market.IsDustQuantity(q, price) {
					log.Infof("%s ignore dust quantity: %f", currency, q.Float64())
					return nil, nil
				}

				q = market.AdjustQuantityByMinNotional(q, price)

				return session, &types.SubmitOrder{
					Symbol:      symbol,
					Side:        side,
					Type:        types.OrderTypeLimit,
					Quantity:    q,
					Price:       price,
					Market:      market,
					TimeInForce: "GTC",
				}

			case types.SideTypeSell:
				baseBalance, ok := session.Account.Balance(currency)
				if !ok {
					continue
				}

				if q.Compare(baseBalance.Available) > 0 {
					log.Warnf("required base amount %f < available base balance %v, skip", q.Float64(), baseBalance)
					continue
				}

				price := ticker.Buy
				if taker {
					price = ticker.Buy
				} else if spread.Compare(market.TickSize) > 0 {
					price = ticker.Buy.Add(market.TickSize)
				} else {
					price = ticker.Sell
				}

				if market.IsDustQuantity(q, price) {
					log.Infof("%s ignore dust quantity: %f", currency, q.Float64())
					return nil, nil
				}

				return session, &types.SubmitOrder{
					Symbol:      symbol,
					Side:        side,
					Type:        types.OrderTypeLimit,
					Quantity:    q,
					Price:       price,
					Market:      market,
					TimeInForce: "GTC",
				}
			}

		}
	}

	return nil, nil
}

func (s *Strategy) CrossRun(ctx context.Context, _ bbgo.OrderExecutionRouter, sessions map[string]*bbgo.ExchangeSession) error {
	instanceID := s.InstanceID()
	_ = instanceID

	s.sessions = make(map[string]*bbgo.ExchangeSession)
	s.orderBooks = make(map[string]*bbgo.ActiveOrderBook)

	for _, sessionName := range s.PreferredSessions {
		session, ok := sessions[sessionName]
		if !ok {
			return fmt.Errorf("incorrect preferred session name: %s is not defined", sessionName)
		}

		orderBook := bbgo.NewActiveOrderBook("")
		orderBook.BindStream(session.UserDataStream)

		s.sessions[sessionName] = session
		s.orderBooks[sessionName] = orderBook
	}

	bbgo.OnShutdown(ctx, func(ctx context.Context, wg *sync.WaitGroup) {
		defer wg.Done()
		for n, session := range s.sessions {
			if ob, ok := s.orderBooks[n]; ok {
				_ = ob.GracefulCancel(ctx, session.Exchange)
			}
		}
	})

	go func() {
		s.align(ctx, s.sessions)

		ticker := time.NewTicker(s.Interval.Duration())
		defer ticker.Stop()

		for {
			select {

			case <-ctx.Done():
				return

			case <-ticker.C:
				s.align(ctx, s.sessions)
			}
		}
	}()

	return nil
}

func (s *Strategy) align(ctx context.Context, sessions map[string]*bbgo.ExchangeSession) {
	totalBalances, sessionBalances := s.aggregateBalances(ctx, sessions)
	_ = sessionBalances

	for sessionName, session := range sessions {
		ob, ok := s.orderBooks[sessionName]
		if !ok {
			log.Errorf("orderbook on session %s not found", sessionName)
			return
		}
		if ok {
			if err := ob.GracefulCancel(ctx, session.Exchange); err != nil {
				log.WithError(err).Errorf("can not cancel order")
			}
		}
	}

	for currency, expectedBalance := range s.ExpectedBalances {
		q := s.calculateRefillQuantity(totalBalances, currency, expectedBalance)

		selectedSession, submitOrder := s.selectSessionForCurrency(ctx, sessions, currency, q)
		if selectedSession != nil && submitOrder != nil {
			log.Infof("placing order on %s: %+v", selectedSession.Name, submitOrder)

			bbgo.Notify("Aligning position", submitOrder)

			if s.DryRun {
				return
			}

			createdOrder, err := selectedSession.Exchange.SubmitOrder(ctx, *submitOrder)
			if err != nil {
				log.WithError(err).Errorf("can not place order")
				return
			}

			if createdOrder != nil {
				if ob, ok := s.orderBooks[selectedSession.Name]; ok {
					ob.Add(*createdOrder)
				} else {
					log.Errorf("orderbook %s not found", selectedSession.Name)
				}
			}
		}
	}
}

func (s *Strategy) calculateRefillQuantity(totalBalances types.BalanceMap, currency string, expectedBalance fixedpoint.Value) fixedpoint.Value {
	if b, ok := totalBalances[currency]; ok {
		netBalance := b.Net()
		return expectedBalance.Sub(netBalance)
	}
	return expectedBalance
}
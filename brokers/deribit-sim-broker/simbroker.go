package deribit_sim_broker

import (
	"errors"
	"fmt"
	"github.com/coinrust/gotrader/data"
	. "github.com/coinrust/gotrader/models"
	"github.com/coinrust/gotrader/util2"
	"log"
	"math"
	"time"
)

const (
	PositionSizeLimit = 1000000 // Position size limit
)

type MarginInfo struct {
	Leverage              float64
	MaintMargin           float64
	LiquidationPriceLong  float64
	LiquidationPriceShort float64
}

// DiribitSimBroker the deribit broker for backtest
type DiribitSimBroker struct {
	data          *data.Data
	makerFeeRate  float64 // -0.00025	// Maker fee rate
	takerFeeRate  float64 // 0.00075	// Taker fee rate
	balance       float64
	orders        map[string]*Order    // All orders key: OrderID value: Order
	openOrders    map[string]*Order    // Open orders
	historyOrders map[string]*Order    // History orders
	positions     map[string]*Position // Position key: symbol
}

func (b *DiribitSimBroker) Subscribe(event string, param string, listener interface{}) {

}

func (b *DiribitSimBroker) GetAccountSummary(currency string) (result AccountSummary, err error) {
	result.Balance = b.balance
	var symbol string
	if currency == "BTC" {
		symbol = "BTC-PERPETUAL"
	} else if currency == "ETH" {
		symbol = "ETH-PERPETUAL"
	}
	position := b.getPosition(symbol)
	var price float64
	tick := b.data.GetTick()
	side := position.Side()
	if side == Buy {
		price = tick.Ask
	} else if side == Sell {
		price = tick.Bid
	}
	pnl, _ := CalcPnl(side, math.Abs(position.Size), position.AvgPrice, price)
	result.Pnl = pnl
	result.Equity = result.Balance + result.Pnl
	return
}

func (b *DiribitSimBroker) GetOrderBook(symbol string, depth int) (result OrderBook, err error) {
	tick := b.data.GetTick()
	result.Time = tick.Timestamp
	result.Asks = []Item{{
		Price:  tick.Ask,
		Amount: float64(tick.AskVolume),
	}}
	result.Bids = []Item{{
		Price:  tick.Bid,
		Amount: float64(tick.BidVolume),
	}}
	return
}

func (b *DiribitSimBroker) PlaceOrder(symbol string, direction Direction, orderType OrderType, price float64,
	stopPx float64, size float64, postOnly bool, reduceOnly bool) (result Order, err error) {
	_id, _ := util2.NextID()
	id := fmt.Sprintf("%v", _id)
	order := &Order{
		ID:           id,
		Symbol:       symbol,
		Price:        price,
		Size:         size,
		AvgPrice:     0,
		FilledAmount: 0,
		Direction:    direction,
		Type:         orderType,
		PostOnly:     postOnly,
		ReduceOnly:   reduceOnly,
		Status:       OrderStatusNew,
	}

	err = b.matchOrder(order, true)
	if err != nil {
		return
	}

	if order.IsOpen() {
		b.openOrders[id] = order
	} else {
		b.historyOrders[id] = order
	}

	b.orders[id] = order
	return
}

// 撮合成交
func (b *DiribitSimBroker) matchOrder(order *Order, immediate bool) (err error) {
	switch order.Type {
	case OrderTypeMarket:
		err = b.matchMarketOrder(order)
	case OrderTypeLimit:
		err = b.matchLimitOrder(order, immediate)
	}
	return
}

func (b *DiribitSimBroker) matchMarketOrder(order *Order) (err error) {
	if !order.IsOpen() {
		return
	}

	// 检查委托:
	// Rejected, maximum size of future position is $1,000,000
	// 开仓总量不能大于 1000000
	// Invalid size - not multiple of contract size ($10)
	// 数量必须是10的整数倍

	if int(order.Size)%10 != 0 {
		err = errors.New("Invalid size - not multiple of contract size ($10)")
		return
	}

	position := b.getPosition(order.Symbol)

	if int(position.Size+order.Size) > PositionSizeLimit ||
		int(position.Size-order.Size) < -PositionSizeLimit {
		err = errors.New("Rejected, maximum size of future position is $1,000,000")
		return
	}

	tick := b.data.GetTick()

	// 判断开仓数量
	margin := b.balance
	// sizeCurrency := order.Amount / price(ask/bid)
	// leverage := sizeCurrency / margin
	// 需要满足: sizeCurrency <= margin * 100
	// 可开仓数量: <= margin * 100 * price(ask/bid)
	var maxSize float64

	// 市价成交
	if order.Direction == Buy {
		maxSize = margin * 100 * tick.Ask
		if order.Size > maxSize {
			err = errors.New(fmt.Sprintf("Rejected, maximum size of future position is %v", maxSize))
			return
		}

		price := tick.Ask
		size := order.Size

		// trade fee
		fee := size / price * b.takerFeeRate

		// Update balance
		b.addBalance(-fee)

		// Update position
		b.updatePosition(order.Symbol, size, price)
	} else if order.Direction == Sell {
		maxSize = margin * 100 * tick.Bid
		if order.Size > maxSize {
			err = errors.New(fmt.Sprintf("Rejected, maximum size of future position is %v", maxSize))
			return
		}

		price := tick.Bid
		size := order.Size

		// trade fee
		fee := size / price * b.takerFeeRate

		// Update balance
		b.addBalance(-fee)

		// Update position
		b.updatePosition(order.Symbol, -size, price)
	}
	return
}

func (b *DiribitSimBroker) matchLimitOrder(order *Order, immediate bool) (err error) {
	if !order.IsOpen() {
		return
	}

	tick := b.data.GetTick()
	if order.Direction == Buy { // Bid order
		if order.Price >= tick.Ask {
			if immediate && order.PostOnly {
				order.Status = OrderStatusRejected
				return
			}

			// match trade
			size := order.Size
			var fee float64

			// trade fee
			if immediate {
				fee = size / order.Price * b.takerFeeRate
			} else {
				fee = size / order.Price * b.makerFeeRate
			}

			// Update balance
			b.addBalance(-fee)

			// Update position
			b.updatePosition(order.Symbol, size, order.Price)
		}
	} else { // Ask order
		if order.Price <= tick.Bid {
			if immediate && order.PostOnly {
				order.Status = OrderStatusRejected
				return
			}

			// match trade
			size := order.Size
			var fee float64

			// trade fee
			if immediate {
				fee = size / order.Price * b.takerFeeRate
			} else {
				fee = size / order.Price * b.makerFeeRate
			}

			// Update balance
			b.addBalance(-fee)

			// Update position
			b.updatePosition(order.Symbol, -size, order.Price)
		}
	}
	return
}

// 更新持仓
func (b *DiribitSimBroker) updatePosition(symbol string, size float64, price float64) {
	position := b.getPosition(symbol)
	if position == nil {
		log.Fatalf("position error symbol=%v", symbol)
	}

	if position.Size > 0 && size < 0 || position.Size < 0 && size > 0 {
		b.closePosition(position, size, price)
	} else {
		b.addPosition(position, size, price)
	}
}

// 增加持仓
func (b *DiribitSimBroker) addPosition(position *Position, size float64, price float64) (err error) {
	if position.Size < 0 && size > 0 || position.Size > 0 && size < 0 {
		err = errors.New("方向错误")
		return
	}
	// 平均成交价
	// total_quantity / ((quantity_1 / price_1) + (quantity_2 / price_2)) = entry_price
	// 增加持仓
	var positionCost float64
	if position.Size != 0 && position.AvgPrice != 0 {
		positionCost = math.Abs(position.Size) / position.AvgPrice
	}

	newPositionCost := math.Abs(size) / price
	totalCost := positionCost + newPositionCost

	totalSize := math.Abs(position.Size + size)
	avgPrice := totalSize / totalCost

	position.AvgPrice = avgPrice
	position.Size += size
	return
}

// 平仓，超过数量，则开立新仓
func (b *DiribitSimBroker) closePosition(position *Position, size float64, price float64) (err error) {
	if position.Size == 0 {
		err = errors.New("当前无持仓")
		return
	}
	if position.Size > 0 && size > 0 || position.Size < 0 && size < 0 {
		err = errors.New("方向错误")
		return
	}
	remaining := math.Abs(size) - math.Abs(position.Size)
	if remaining > 0 {
		// 先平掉原有持仓
		// 计算盈利
		pnl, _ := CalcPnl(position.Side(), math.Abs(position.Size), position.AvgPrice, price)
		b.addPnl(pnl)
		position.AvgPrice = price
		position.Size = position.Size + size
	} else if remaining == 0 {
		// 完全平仓
		pnl, _ := CalcPnl(position.Side(), math.Abs(size), position.AvgPrice, price)
		b.addPnl(pnl)
		position.AvgPrice = 0
		position.Size = 0
	} else {
		// 部分平仓
		pnl, _ := CalcPnl(position.Side(), math.Abs(position.Size), position.AvgPrice, price)
		b.addPnl(pnl)
		//position.AvgPrice = position.AvgPrice
		position.Size = position.Size + size
	}
	return
}

// 增加Balance
func (b *DiribitSimBroker) addBalance(value float64) {
	b.balance += value
}

// 增加P/L
func (b *DiribitSimBroker) addPnl(pnl float64) {
	b.balance += pnl
}

// 获取持仓
func (b *DiribitSimBroker) getPosition(symbol string) *Position {
	if position, ok := b.positions[symbol]; ok {
		return position
	} else {
		position = &Position{
			Symbol:    symbol,
			OpenI:     time.Time{},
			OpenPrice: 0,
			Size:      0,
			AvgPrice:  0,
		}
		b.positions[symbol] = position
		return position
	}
}

func (b *DiribitSimBroker) GetOpenOrders(symbol string) (result []Order, err error) {
	for _, v := range b.openOrders {
		if v.Symbol == symbol {
			result = append(result, *v)
		}
	}
	return
}

func (b *DiribitSimBroker) GetOrder(symbol string, id string) (result Order, err error) {
	order, ok := b.orders[id]
	if !ok {
		err = errors.New("not found")
		return
	}
	result = *order
	return
}

func (b *DiribitSimBroker) CancelOrder(symbol string, id string) (result Order, err error) {
	if order, ok := b.orders[id]; ok {
		if !order.IsOpen() {
			err = errors.New("status error")
			return
		}
		switch order.Status {
		case OrderStatusCreated, OrderStatusNew, OrderStatusPartiallyFilled:
			order.Status = OrderStatusCancelled
			result = *order
			delete(b.openOrders, id)
		default:
			err = errors.New("error")
		}
	} else {
		err = errors.New("not found")
	}
	return
}

func (b *DiribitSimBroker) CancelAllOrders(symbol string) (err error) {
	var idsToBeRemoved []string

	for _, order := range b.openOrders {
		if !order.IsOpen() {
			log.Printf("Order error: %#v", order)
			continue
		}
		switch order.Status {
		case OrderStatusCreated, OrderStatusNew, OrderStatusPartiallyFilled:
			order.Status = OrderStatusCancelled
			idsToBeRemoved = append(idsToBeRemoved, order.ID)
		default:
			err = errors.New("error")
		}
	}

	for _, id := range idsToBeRemoved {
		delete(b.openOrders, id)
	}
	return
}

func (b *DiribitSimBroker) AmendOrder(symbol string, id string, price float64, size float64) (result Order, err error) {
	return
}

func (b *DiribitSimBroker) GetPosition(symbol string) (result Position, err error) {
	position, ok := b.positions[symbol]
	if !ok {
		err = errors.New("not found")
		return
	}
	result = *position
	return
}

func (b *DiribitSimBroker) RunEventLoopOnce() (err error) {
	for _, order := range b.openOrders {
		b.matchOrder(order, false)
	}
	return
}

func NewBroker(data *data.Data, cash float64, makerFeeRate float64, takerFeeRate float64) *DiribitSimBroker {
	return &DiribitSimBroker{
		data:          data,
		balance:       cash,
		makerFeeRate:  makerFeeRate, // -0.00025 // Maker 费率
		takerFeeRate:  takerFeeRate, // 0.00075	// Taker 费率
		orders:        make(map[string]*Order),
		openOrders:    make(map[string]*Order),
		historyOrders: make(map[string]*Order),
		positions:     make(map[string]*Position),
	}
}

package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/rand"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/garyburd/redigo/redis"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/middleware"
)

type User struct {
	ID        int64  `json:"id,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	LoginName string `json:"login_name,omitempty"`
	PassHash  string `json:"pass_hash,omitempty"`
}

type Event struct {
	ID       int64  `json:"id,omitempty"`
	Title    string `json:"title,omitempty"`
	PublicFg bool   `json:"public,omitempty"`
	ClosedFg bool   `json:"closed,omitempty"`
	Price    int64  `json:"price,omitempty"`

	Total   int                `json:"total"`
	Remains int                `json:"remains"`
	Sheets  map[string]*Sheets `json:"sheets,omitempty"`
}

type Sheets struct {
	Total   int      `json:"total"`
	Remains int      `json:"remains"`
	Detail  []*Sheet `json:"detail,omitempty"`
	Price   int64    `json:"price"`
}

type Sheet struct {
	ID    int64  `json:"-"`
	Rank  string `json:"-"`
	Num   int64  `json:"num"`
	Price int64  `json:"-"`

	Mine           bool       `json:"mine,omitempty"`
	Reserved       bool       `json:"reserved,omitempty"`
	ReservedAt     *time.Time `json:"-"`
	ReservedAtUnix int64      `json:"reserved_at,omitempty"`
}

type Reservation struct {
	ID         int64      `json:"id"`
	EventID    int64      `json:"-"`
	SheetID    int64      `json:"-"`
	UserID     int64      `json:"-"`
	ReservedAt *time.Time `json:"-"`
	CanceledAt *time.Time `json:"-"`

	Event          *Event `json:"event,omitempty"`
	SheetRank      string `json:"sheet_rank,omitempty"`
	SheetNum       int64  `json:"sheet_num,omitempty"`
	Price          int64  `json:"price,omitempty"`
	ReservedAtUnix int64  `json:"reserved_at,omitempty"`
	CanceledAtUnix int64  `json:"canceled_at,omitempty"`
}

type Administrator struct {
	ID        int64  `json:"id,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	LoginName string `json:"login_name,omitempty"`
	PassHash  string `json:"pass_hash,omitempty"`
}

var (
	kvsPool *redis.Pool

	sheetsRanks = []string{"S", "A", "B", "C"}
	sheetsMap   = map[string]int64{
		"S": 50,
		"A": 150,
		"B": 300,
		"C": 500,
	}
	priceMap = map[string]int64{
		"S": 5000,
		"A": 3000,
		"B": 1000,
		"C": 0,
	}
	sheetsMapById              []*Sheet
	sheetsMapByRankAndNum      map[string]map[int64]*Sheet
	sheetsMapByRankSortedByNum map[string][]*Sheet
)

func calcPassHash(password string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(password)))
}

func getKvsKeyForFreeSheets(eventID int64, rank string) string {
	return fmt.Sprintf("freeSheets:%d:%s", eventID, rank)
}

func getKvsKeyForEvent(eventID int64) string {
	return fmt.Sprintf("event:%d", eventID)
}

func newKvsPool(addr string) *redis.Pool {
	return &redis.Pool{
		MaxIdle:     5,
		IdleTimeout: 240 * time.Second,
		// Dial or DialContext must be set. When both are set, DialContext takes precedence over Dial.
		Dial: func() (redis.Conn, error) {
			c, err := redis.Dial("tcp", addr)
			if err != nil {
				panic(err.Error())
			}
			return c, err
		},
	}
}

func initEventStates(kvs redis.Conn, events []*Event) error {
	args := redis.Args{}
	for _, event := range events {
		eventJson, err := json.Marshal(event)
		if err != nil {
			return err
		}
		args = args.Add(getKvsKeyForEvent(event.ID)).Add(eventJson)
	}
	if _, err := kvs.Do("MSET", args...); err != nil {
		return err
	}
	for rank, sheetsByRank := range sheetsMapByRankSortedByNum {
		sheetsByRankShuffle := make([]*Sheet, len(sheetsByRank))
		for _, event := range events {
			copy(sheetsByRankShuffle, sheetsByRank)
			rand.Shuffle(len(sheetsByRankShuffle), func(i, j int) {
				sheetsByRankShuffle[i], sheetsByRankShuffle[j] = sheetsByRankShuffle[j], sheetsByRankShuffle[i]
			})
			args = redis.Args{}.Add(getKvsKeyForFreeSheets(event.ID, rank))
			for _, s := range sheetsByRankShuffle {
				args = args.Add(s.Num)
			}
			if _, err := kvs.Do("LPUSH", args...); err != nil {
				return err
			}
		}
	}
	return nil
}

func initSheetsCache() error {
	sheetsMapById = make([]*Sheet, 1001)
	sheetsMapByRankAndNum = map[string]map[int64]*Sheet{}
	rows, err := db.Query("SELECT * FROM sheets order by `rank`, num")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sheet Sheet
		if err := rows.Scan(&sheet.ID, &sheet.Rank, &sheet.Num, &sheet.Price); err != nil {
			return err
		}
		sheetsMapById[sheet.ID] = &sheet
		if sheetsMapByRankAndNum[sheet.Rank] == nil {
			sheetsMapByRankAndNum[sheet.Rank] = map[int64]*Sheet{}
		}
		sheetsMapByRankAndNum[sheet.Rank][sheet.Num] = &sheet
	}

	sheetsMapByRankSortedByNum = map[string][]*Sheet{}
	for rank, sheetsByRank := range sheetsMapByRankAndNum {
		sheetsMapByRankSortedByNum[rank] = make([]*Sheet, len(sheetsByRank))
		nums := make([]int64, len(sheetsByRank))
		i := 0
		for num := range sheetsByRank {
			nums[i] = num
			i++
		}
		sort.Slice(nums, func(i, j int) bool {
			return nums[i] < nums[j]
		})
		for i, num := range nums {
			sheetsMapByRankSortedByNum[rank][i] = sheetsByRank[num]
		}
	}

	return nil
}

func sessUserID(c echo.Context) int64 {
	sess, _ := session.Get("session", c)
	var userID int64
	if x, ok := sess.Values["user_id"]; ok {
		userID, _ = x.(int64)
	}
	return userID
}

func sessSetUserID(c echo.Context, id int64) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	sess.Values["user_id"] = id
	sess.Save(c.Request(), c.Response())
}

func sessDeleteUserID(c echo.Context) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	delete(sess.Values, "user_id")
	sess.Save(c.Request(), c.Response())
}

func sessAdministratorID(c echo.Context) int64 {
	sess, _ := session.Get("session", c)
	var administratorID int64
	if x, ok := sess.Values["administrator_id"]; ok {
		administratorID, _ = x.(int64)
	}
	return administratorID
}

func sessSetAdministratorID(c echo.Context, id int64) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	sess.Values["administrator_id"] = id
	sess.Save(c.Request(), c.Response())
}

func sessDeleteAdministratorID(c echo.Context) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	delete(sess.Values, "administrator_id")
	sess.Save(c.Request(), c.Response())
}

func loginRequired(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if _, err := getLoginUser(c); err != nil {
			return resError(c, "login_required", 401)
		}
		return next(c)
	}
}

func adminLoginRequired(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if _, err := getLoginAdministrator(c); err != nil {
			return resError(c, "admin_login_required", 401)
		}
		return next(c)
	}
}

func getLoginUser(c echo.Context) (*User, error) {
	userID := sessUserID(c)
	if userID == 0 {
		return nil, errors.New("not logged in")
	}
	var user User
	err := db.QueryRow("SELECT id, nickname FROM users WHERE id = ?", userID).Scan(&user.ID, &user.Nickname)
	return &user, err
}

func getLoginAdministrator(c echo.Context) (*Administrator, error) {
	administratorID := sessAdministratorID(c)
	if administratorID == 0 {
		return nil, errors.New("not logged in")
	}
	var administrator Administrator
	err := db.QueryRow("SELECT id, nickname FROM administrators WHERE id = ?", administratorID).Scan(&administrator.ID, &administrator.Nickname)
	return &administrator, err
}

func getEvents(all bool) ([]*Event, error) {
	var sql string
	if all {
		sql = "SELECT * FROM events ORDER BY id ASC"
	} else {
		sql = "SELECT * FROM events WHERE public_fg = 1 ORDER BY id ASC"
	}
	rows, err := db.Query(sql)
	if err != nil {
		return nil, err
	}
	var events []*Event
	var eventIds []interface{}
	reservations := map[int64]map[int64]*Reservation{}
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.Title, &event.PublicFg, &event.ClosedFg, &event.Price); err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, &event)
		eventIds = append(eventIds, event.ID)
		reservations[event.ID] = map[int64]*Reservation{}
	}
	rows.Close()
	rows, err = db.Query("SELECT event_id, sheet_id, user_id, reserved_at FROM reservations WHERE event_id IN (?"+strings.Repeat(",?", len(eventIds)-1)+") and canceled_at IS NULL", eventIds...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var reservation Reservation
		if err := rows.Scan(&reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt); err != nil {
			rows.Close()
			return nil, err
		}
		reservations[reservation.EventID][reservation.SheetID] = &reservation
	}
	rows.Close()
	for _, event := range events {
		event.Total = 0
		event.Remains = 0
		event.Sheets = map[string]*Sheets{}
		for rank, sheetsByRank := range sheetsMapByRankSortedByNum {
			event.Sheets[rank] = &Sheets{}
			for _, sheet := range sheetsByRank {
				s := &Sheet{ID: sheet.ID, Rank: rank, Num: sheet.Num, Price: sheet.Price}
				event.Sheets[rank].Price = event.Price + s.Price
				event.Total++
				event.Sheets[rank].Total++
				reservation, ok := reservations[event.ID][sheet.ID]
				if ok {
					s.Mine = false
					s.Reserved = true
					s.ReservedAtUnix = reservation.ReservedAt.Unix()
				} else {
					event.Remains++
					event.Sheets[rank].Remains++
				}
				event.Sheets[rank].Detail = append(event.Sheets[rank].Detail, s)
			}
		}
	}
	return events, nil
}

func getBaseEvent(eventID int64) (*Event, error) {
	kvs := kvsPool.Get()
	defer kvs.Close()
	eventJson, err := redis.String(kvs.Do("GET", getKvsKeyForEvent(eventID)))
	if err != nil {
		return nil, err
	}
	var event Event
	if err = json.Unmarshal([]byte(eventJson), &event); err != nil {
		return nil, err
	}
	return &event, nil
}

func getEvent(eventID, loginUserID int64) (*Event, error) {
	event, err := getBaseEvent(eventID)
	if err != nil {
		return nil, err
	}
	reservations := map[int64]*Reservation{}
	rows, err := db.Query("SELECT sheet_id, user_id, reserved_at FROM reservations WHERE event_id = ? AND canceled_at IS NULL", event.ID)
	if err != nil {
		if err != sql.ErrNoRows {
			return nil, err
		}
	} else {
		for rows.Next() {
			var reservation Reservation
			if err := rows.Scan(&reservation.SheetID, &reservation.UserID, &reservation.ReservedAt); err != nil {
				rows.Close()
				return nil, err
			}
			reservations[reservation.SheetID] = &reservation
		}
		rows.Close()
	}
	event.Total = 0
	event.Remains = 0
	event.Sheets = map[string]*Sheets{}
	for rank, sheetsByRank := range sheetsMapByRankSortedByNum {
		event.Sheets[rank] = &Sheets{}
		for _, sheet := range sheetsByRank {
			s := &Sheet{ID: sheet.ID, Rank: rank, Num: sheet.Num, Price: sheet.Price}
			event.Sheets[rank].Price = event.Price + s.Price
			event.Total++
			event.Sheets[rank].Total++
			reservation, ok := reservations[sheet.ID]
			if ok {
				s.Mine = reservation.UserID == loginUserID
				s.Reserved = true
				s.ReservedAtUnix = reservation.ReservedAt.Unix()
			} else {
				event.Remains++
				event.Sheets[rank].Remains++
			}
			event.Sheets[rank].Detail = append(event.Sheets[rank].Detail, s)
		}
	}

	return event, nil
}

func sanitizeEvent(e *Event) *Event {
	sanitized := *e
	sanitized.Price = 0
	sanitized.PublicFg = false
	sanitized.ClosedFg = false
	return &sanitized
}

func fillinUser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if user, err := getLoginUser(c); err == nil {
			c.Set("user", user)
		}
		return next(c)
	}
}

func fillinAdministrator(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if administrator, err := getLoginAdministrator(c); err == nil {
			c.Set("administrator", administrator)
		}
		return next(c)
	}
}

func validateRank(rank string) bool {
	sheetsByRank, ok := sheetsMapByRankAndNum[rank]
	if !ok {
		return false
	}
	return len(sheetsByRank) > 0
}

type Renderer struct {
	templates *template.Template
}

func (r *Renderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return r.templates.ExecuteTemplate(w, name, data)
}

var db *sql.DB

func main() {
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4",
		os.Getenv("DB_USER"), os.Getenv("DB_PASS"),
		os.Getenv("DB_HOST"), os.Getenv("DB_PORT"),
		os.Getenv("DB_DATABASE"),
	)

	var err error
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}

	kvsAddr := fmt.Sprintf("%s:%s", os.Getenv("KVS_HOST"), os.Getenv("KVS_PORT"))
	kvsPool = newKvsPool(kvsAddr)

	e := echo.New()
	funcs := template.FuncMap{
		"encode_json": func(v interface{}) string {
			b, _ := json.Marshal(v)
			return string(b)
		},
	}
	e.Renderer = &Renderer{
		templates: template.Must(template.New("").Delims("[[", "]]").Funcs(funcs).ParseGlob("views/*.tmpl")),
	}
	e.Use(session.Middleware(sessions.NewCookieStore([]byte("secret"))))
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{Output: os.Stderr}))
	e.Static("/", "public")
	e.GET("/", func(c echo.Context) error {
		events, err := getEvents(false)
		if err != nil {
			return err
		}
		for i, v := range events {
			events[i] = sanitizeEvent(v)
		}
		return c.Render(200, "index.tmpl", echo.Map{
			"events": events,
			"user":   c.Get("user"),
			"origin": c.Scheme() + "://" + c.Request().Host,
		})
	}, fillinUser)
	e.GET("/initialize", func(c echo.Context) error {
		kvs := kvsPool.Get()
		defer kvs.Close()
		kvs.Do("FLUSHALL")

		cmd := exec.Command("../../db/init.sh")
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		err := cmd.Run()
		if err != nil {
			return nil
		}

		if err = initSheetsCache(); err != nil {
			return err
		}

		events := []*Event{}
		rows, err := db.Query("SELECT * FROM events")
		if err != nil {
			return err
		}
		for rows.Next() {
			var event Event
			if err := rows.Scan(&event.ID, &event.Title, &event.PublicFg, &event.ClosedFg, &event.Price); err != nil {
				rows.Close()
				return err
			}
			events = append(events, &event)
		}
		rows.Close()
		if err = initEventStates(kvs, events); err != nil {
			return err
		}
		rows, err = db.Query("SELECT event_id, sheet_id FROM reservations WHERE canceled_at IS NULL")
		if err != nil {
			return err
		}
		for rows.Next() {
			var eventID, sheetID int64
			if err = rows.Scan(&eventID, &sheetID); err != nil {
				rows.Close()
				return err
			}
			sheet := sheetsMapById[sheetID]
			_, err = kvs.Do("LREM", getKvsKeyForFreeSheets(eventID, sheet.Rank), 0, sheet.Num)
			if err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()

		return c.NoContent(204)
	})
	e.POST("/api/users", func(c echo.Context) error {
		var params struct {
			Nickname  string `json:"nickname"`
			LoginName string `json:"login_name"`
			Password  string `json:"password"`
		}
		c.Bind(&params)

		tx, err := db.Begin()
		if err != nil {
			return err
		}

		var user User
		if err := tx.QueryRow("SELECT * FROM users WHERE login_name = ?", params.LoginName).Scan(&user.ID, &user.LoginName, &user.Nickname, &user.PassHash); err != sql.ErrNoRows {
			tx.Rollback()
			if err == nil {
				return resError(c, "duplicated", 409)
			}
			return err
		}

		res, err := tx.Exec("INSERT INTO users (login_name, pass_hash, nickname) VALUES (?, ?, ?)", params.LoginName, calcPassHash(params.Password), params.Nickname)
		if err != nil {
			tx.Rollback()
			return resError(c, "", 0)
		}
		userID, err := res.LastInsertId()
		if err != nil {
			tx.Rollback()
			return resError(c, "", 0)
		}
		if err := tx.Commit(); err != nil {
			return err
		}

		return c.JSON(201, echo.Map{
			"id":       userID,
			"nickname": params.Nickname,
		})
	})
	e.GET("/api/users/:id", func(c echo.Context) error {
		var user User
		if err := db.QueryRow("SELECT id, nickname FROM users WHERE id = ?", c.Param("id")).Scan(&user.ID, &user.Nickname); err != nil {
			return err
		}

		loginUser, err := getLoginUser(c)
		if err != nil {
			return err
		}
		if user.ID != loginUser.ID {
			return resError(c, "forbidden", 403)
		}

		rows, err := db.Query("SELECT * FROM reservations WHERE user_id = ? ORDER BY IFNULL(canceled_at, reserved_at) DESC LIMIT 5", user.ID)
		if err != nil {
			return err
		}
		defer rows.Close()

		var recentReservations []Reservation
		for rows.Next() {
			var reservation Reservation
			if err := rows.Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt); err != nil {
				return err
			}
			sheet := sheetsMapById[reservation.SheetID]

			event, err := getEvent(reservation.EventID, -1)
			if err != nil {
				return err
			}
			price := event.Sheets[sheet.Rank].Price
			event.Sheets = nil
			event.Total = 0
			event.Remains = 0

			reservation.Event = event
			reservation.SheetRank = sheet.Rank
			reservation.SheetNum = sheet.Num
			reservation.Price = price
			reservation.ReservedAtUnix = reservation.ReservedAt.Unix()
			if reservation.CanceledAt != nil {
				reservation.CanceledAtUnix = reservation.CanceledAt.Unix()
			}
			recentReservations = append(recentReservations, reservation)
		}
		if recentReservations == nil {
			recentReservations = make([]Reservation, 0)
		}

		var totalPrice int
		if err := db.QueryRow("SELECT IFNULL(SUM(e.price + s.price), 0) FROM reservations r INNER JOIN sheets s ON s.id = r.sheet_id INNER JOIN events e ON e.id = r.event_id WHERE r.user_id = ? AND r.canceled_at IS NULL", user.ID).Scan(&totalPrice); err != nil {
			return err
		}

		rows, err = db.Query("SELECT event_id FROM reservations WHERE user_id = ? GROUP BY event_id ORDER BY MAX(IFNULL(canceled_at, reserved_at)) DESC LIMIT 5", user.ID)
		if err != nil {
			return err
		}
		defer rows.Close()

		var recentEvents []*Event
		for rows.Next() {
			var eventID int64
			if err := rows.Scan(&eventID); err != nil {
				return err
			}
			event, err := getEvent(eventID, -1)
			if err != nil {
				return err
			}
			for k := range event.Sheets {
				event.Sheets[k].Detail = nil
			}
			recentEvents = append(recentEvents, event)
		}
		if recentEvents == nil {
			recentEvents = make([]*Event, 0)
		}

		return c.JSON(200, echo.Map{
			"id":                  user.ID,
			"nickname":            user.Nickname,
			"recent_reservations": recentReservations,
			"total_price":         totalPrice,
			"recent_events":       recentEvents,
		})
	}, loginRequired)
	e.POST("/api/actions/login", func(c echo.Context) error {
		var params struct {
			LoginName string `json:"login_name"`
			Password  string `json:"password"`
		}
		c.Bind(&params)

		user := new(User)
		if err := db.QueryRow("SELECT * FROM users WHERE login_name = ?", params.LoginName).Scan(&user.ID, &user.LoginName, &user.Nickname, &user.PassHash); err != nil {
			if err == sql.ErrNoRows {
				return resError(c, "authentication_failed", 401)
			}
			return err
		}

		if user.PassHash != calcPassHash(params.Password) {
			return resError(c, "authentication_failed", 401)
		}

		sessSetUserID(c, user.ID)
		user, err = getLoginUser(c)
		if err != nil {
			return err
		}
		return c.JSON(200, user)
	})
	e.POST("/api/actions/logout", func(c echo.Context) error {
		sessDeleteUserID(c)
		return c.NoContent(204)
	}, loginRequired)
	e.GET("/api/events", func(c echo.Context) error {
		events, err := getEvents(true)
		if err != nil {
			return err
		}
		for i, v := range events {
			events[i] = sanitizeEvent(v)
		}
		return c.JSON(200, events)
	})
	e.GET("/api/events/:id", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}

		loginUserID := int64(-1)
		if user, err := getLoginUser(c); err == nil {
			loginUserID = user.ID
		}

		event, err := getEvent(eventID, loginUserID)
		if err != nil {
			if err == sql.ErrNoRows || err == redis.ErrNil {
				return resError(c, "not_found", 404)
			}
			return err
		} else if !event.PublicFg {
			return resError(c, "not_found", 404)
		}
		return c.JSON(200, sanitizeEvent(event))
	})
	e.POST("/api/events/:id/actions/reserve", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}
		var params struct {
			Rank string `json:"sheet_rank"`
		}
		c.Bind(&params)

		user, err := getLoginUser(c)
		if err != nil {
			return err
		}

		event, err := getBaseEvent(eventID)
		if err != nil {
			if err == sql.ErrNoRows || err == redis.ErrNil {
				return resError(c, "invalid_event", 404)
			}
			return err
		} else if !event.PublicFg {
			return resError(c, "invalid_event", 404)
		}

		if !validateRank(params.Rank) {
			return resError(c, "invalid_rank", 400)
		}

		kvs := kvsPool.Get()
		defer kvs.Close()

		tx, err := db.Begin()
		if err != nil {
			return err
		}
		num, err := redis.Int64(kvs.Do("LPOP", getKvsKeyForFreeSheets(event.ID, params.Rank)))
		if err != nil {
			tx.Rollback()
			if err == redis.ErrNil {
				return resError(c, "sold_out", 409)
			}
			return err
		}
		sheet := sheetsMapByRankAndNum[params.Rank][num]
		res, err := tx.Exec("INSERT INTO reservations (event_id, sheet_id, user_id, reserved_at) VALUES (?, ?, ?, ?)", event.ID, sheet.ID, user.ID, time.Now().UTC().Format("2006-01-02 15:04:05.000000"))
		if err != nil {
			tx.Rollback()
			kvs.Do("RPUSH", getKvsKeyForFreeSheets(event.ID, params.Rank), num)
			return err
		}
		reservationID, err := res.LastInsertId()
		if err != nil {
			tx.Rollback()
			kvs.Do("RPUSH", getKvsKeyForFreeSheets(event.ID, params.Rank), num)
			return err
		}
		if err := tx.Commit(); err != nil {
			kvs.Do("RPUSH", getKvsKeyForFreeSheets(event.ID, params.Rank), num)
			return err
		}

		return c.JSON(202, echo.Map{
			"id":         reservationID,
			"sheet_rank": params.Rank,
			"sheet_num":  sheet.Num,
		})
	}, loginRequired)
	e.DELETE("/api/events/:id/sheets/:rank/:num/reservation", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}
		rank := c.Param("rank")
		num, err := strconv.ParseInt(c.Param("num"), 10, 64)
		if err != nil {
			return err
		}

		user, err := getLoginUser(c)
		if err != nil {
			return err
		}

		event, err := getBaseEvent(eventID)
		if err != nil {
			if err == sql.ErrNoRows || err == redis.ErrNil {
				return resError(c, "invalid_event", 404)
			}
			return err
		} else if !event.PublicFg {
			return resError(c, "invalid_event", 404)
		}

		if !validateRank(rank) {
			return resError(c, "invalid_rank", 404)
		}

		sheet, ok := sheetsMapByRankAndNum[rank][num]
		if !ok {
			return resError(c, "invalid_sheet", 404)
		}

		kvs := kvsPool.Get()
		defer kvs.Close()

		tx, err := db.Begin()
		if err != nil {
			return err
		}

		var reservation Reservation
		if err := tx.QueryRow("SELECT * FROM reservations WHERE event_id = ? AND sheet_id = ? AND canceled_at IS NULL FOR UPDATE", event.ID, sheet.ID).Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt); err != nil {
			tx.Rollback()
			if err == sql.ErrNoRows {
				return resError(c, "not_reserved", 400)
			}
			return err
		}
		if reservation.UserID != user.ID {
			tx.Rollback()
			return resError(c, "not_permitted", 403)
		}

		if _, err := kvs.Do("RPUSH", getKvsKeyForFreeSheets(event.ID, sheet.Rank), sheet.Num); err != nil {
			tx.Rollback()
			return err
		}

		if _, err := tx.Exec("UPDATE reservations SET canceled_at = ? WHERE id = ?", time.Now().UTC().Format("2006-01-02 15:04:05.000000"), reservation.ID); err != nil {
			tx.Rollback()
			kvs.Do("RPOP", getKvsKeyForFreeSheets(event.ID, sheet.Rank))
			return err
		}

		if err := tx.Commit(); err != nil {
			kvs.Do("RPOP", getKvsKeyForFreeSheets(event.ID, sheet.Rank))
			return err
		}

		return c.NoContent(204)
	}, loginRequired)
	e.GET("/admin/", func(c echo.Context) error {
		var events []*Event
		administrator := c.Get("administrator")
		if administrator != nil {
			var err error
			if events, err = getEvents(true); err != nil {
				return err
			}
		}
		return c.Render(200, "admin.tmpl", echo.Map{
			"events":        events,
			"administrator": administrator,
			"origin":        c.Scheme() + "://" + c.Request().Host,
		})
	}, fillinAdministrator)
	e.POST("/admin/api/actions/login", func(c echo.Context) error {
		var params struct {
			LoginName string `json:"login_name"`
			Password  string `json:"password"`
		}
		c.Bind(&params)

		administrator := new(Administrator)
		if err := db.QueryRow("SELECT * FROM administrators WHERE login_name = ?", params.LoginName).Scan(&administrator.ID, &administrator.LoginName, &administrator.Nickname, &administrator.PassHash); err != nil {
			if err == sql.ErrNoRows {
				return resError(c, "authentication_failed", 401)
			}
			return err
		}

		if administrator.PassHash != calcPassHash(params.Password) {
			return resError(c, "authentication_failed", 401)
		}

		sessSetAdministratorID(c, administrator.ID)
		administrator, err = getLoginAdministrator(c)
		if err != nil {
			return err
		}
		return c.JSON(200, administrator)
	})
	e.POST("/admin/api/actions/logout", func(c echo.Context) error {
		sessDeleteAdministratorID(c)
		return c.NoContent(204)
	}, adminLoginRequired)
	e.GET("/admin/api/events", func(c echo.Context) error {
		events, err := getEvents(true)
		if err != nil {
			return err
		}
		return c.JSON(200, events)
	}, adminLoginRequired)
	e.POST("/admin/api/events", func(c echo.Context) error {
		var params struct {
			Title  string `json:"title"`
			Public bool   `json:"public"`
			Price  int    `json:"price"`
		}
		c.Bind(&params)
		event := &Event{
			Title:    params.Title,
			PublicFg: params.Public,
			ClosedFg: false,
			Price:    int64(params.Price),
			Total:    1000,
			Remains:  1000,
			Sheets:   map[string]*Sheets{},
		}
		for _, rank := range sheetsRanks {
			event.Sheets[rank] = &Sheets{
				Total:   int(sheetsMap[rank]),
				Remains: int(sheetsMap[rank]),
				Price:   event.Price + priceMap[rank],
			}
		}

		kvs := kvsPool.Get()
		defer kvs.Close()

		tx, err := db.Begin()
		if err != nil {
			return err
		}

		res, err := tx.Exec("INSERT INTO events (title, public_fg, closed_fg, price) VALUES (?, ?, 0, ?)", params.Title, params.Public, params.Price)
		if err != nil {
			tx.Rollback()
			return err
		}
		event.ID, err = res.LastInsertId()
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := initEventStates(kvs, []*Event{event}); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}

		return c.JSON(200, *event)
	}, adminLoginRequired)
	e.GET("/admin/api/events/:id", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}
		event, err := getEvent(eventID, -1)
		if err != nil {
			if err == sql.ErrNoRows || err == redis.ErrNil {
				return resError(c, "not_found", 404)
			}
			return err
		}
		return c.JSON(200, event)
	}, adminLoginRequired)
	e.POST("/admin/api/events/:id/actions/edit", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}

		var params struct {
			Public bool `json:"public"`
			Closed bool `json:"closed"`
		}
		c.Bind(&params)
		if params.Closed {
			params.Public = false
		}

		event, err := getBaseEvent(eventID)
		if err != nil {
			if err == sql.ErrNoRows || err == redis.ErrNil {
				return resError(c, "not_found", 404)
			}
			return err
		}

		if event.ClosedFg {
			return resError(c, "cannot_edit_closed_event", 400)
		} else if event.PublicFg && params.Closed {
			return resError(c, "cannot_close_public_event", 400)
		}

		event.PublicFg = params.Public
		event.ClosedFg = params.Closed

		kvs := kvsPool.Get()
		defer kvs.Close()
		eventJson, err := json.Marshal(event)
		if err != nil {
			return err
		}

		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE events SET public_fg = ?, closed_fg = ? WHERE id = ?", event.PublicFg, event.ClosedFg, event.ID); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := kvs.Do("SET", getKvsKeyForEvent(event.ID), eventJson); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}

		c.JSON(200, event)
		return nil
	}, adminLoginRequired)
	e.GET("/admin/api/reports/events/:id/sales", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}

		event, err := getBaseEvent(eventID)
		if err != nil {
			return err
		}

		rows, err := db.Query("SELECT * FROM reservations WHERE event_id = ? ORDER BY id ASC", event.ID)
		if err != nil {
			return err
		}
		defer rows.Close()

		var reports []Report
		for rows.Next() {
			var reservation Reservation
			if err := rows.Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt); err != nil {
				return err
			}
			sheet := sheetsMapById[reservation.SheetID]
			report := Report{
				ReservationID: reservation.ID,
				EventID:       event.ID,
				Rank:          sheet.Rank,
				Num:           sheet.Num,
				UserID:        reservation.UserID,
				SoldAt:        reservation.ReservedAt.Format("2006-01-02T15:04:05.000000Z"),
				Price:         event.Price + sheet.Price,
			}
			if reservation.CanceledAt != nil {
				report.CanceledAt = reservation.CanceledAt.Format("2006-01-02T15:04:05.000000Z")
			}
			reports = append(reports, report)
		}
		return renderReportCSV(c, reports)
	}, adminLoginRequired)
	e.GET("/admin/api/reports/sales", func(c echo.Context) error {
		rows, err := db.Query("SELECT r.*, e.price AS event_price FROM reservations r INNER JOIN events e ON e.id = r.event_id ORDER BY id ASC")
		if err != nil {
			return err
		}
		defer rows.Close()

		var reports []Report
		for rows.Next() {
			var reservation Reservation
			var eventPrice int64
			if err := rows.Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt, &eventPrice); err != nil {
				return err
			}
			sheet := sheetsMapById[reservation.SheetID]
			report := Report{
				ReservationID: reservation.ID,
				EventID:       reservation.EventID,
				Rank:          sheet.Rank,
				Num:           sheet.Num,
				UserID:        reservation.UserID,
				SoldAt:        reservation.ReservedAt.Format("2006-01-02T15:04:05.000000Z"),
				Price:         eventPrice + sheet.Price,
			}
			if reservation.CanceledAt != nil {
				report.CanceledAt = reservation.CanceledAt.Format("2006-01-02T15:04:05.000000Z")
			}
			reports = append(reports, report)
		}
		return renderReportCSV(c, reports)
	}, adminLoginRequired)

	e.Start(":8080")
}

type Report struct {
	ReservationID int64
	EventID       int64
	Rank          string
	Num           int64
	UserID        int64
	SoldAt        string
	CanceledAt    string
	Price         int64
}

func renderReportCSV(c echo.Context, reports []Report) error {
	sort.Slice(reports, func(i, j int) bool { return strings.Compare(reports[i].SoldAt, reports[j].SoldAt) < 0 })

	body := bytes.NewBufferString("reservation_id,event_id,rank,num,price,user_id,sold_at,canceled_at\n")
	for _, v := range reports {
		body.WriteString(fmt.Sprintf("%d,%d,%s,%d,%d,%d,%s,%s\n",
			v.ReservationID, v.EventID, v.Rank, v.Num, v.Price, v.UserID, v.SoldAt, v.CanceledAt))
	}

	c.Response().Header().Set("Content-Type", `text/csv; charset=UTF-8`)
	c.Response().Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
	_, err := io.Copy(c.Response(), body)
	return err
}

func resError(c echo.Context, e string, status int) error {
	if e == "" {
		e = "unknown"
	}
	if status < 100 {
		status = 500
	}
	return c.JSON(status, map[string]string{"error": e})
}

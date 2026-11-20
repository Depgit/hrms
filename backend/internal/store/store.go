// Package store is the only way the app reaches MongoDB: one collection per former PostgreSQL table, a write
// wrapper that keeps the old database's rules (defaults, NOT NULL, CHECK, foreign keys, the append-only audit log,
// Postgres-like error codes) from the schema registry in schema_gen.go, and transactions with automatic retry.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// Row is a JSON-ready document: every column of the collection (NULL ones as nil), `_id` read back as `id`
// (dropped when it is an automatic ObjectID), int4 → int32, int8 → int64, numeric → float64, date → "YYYY-MM-DD",
// time → "HH:MM", timestamptz → time.Time in the business zone, jsonb → map[string]any / []any.
type Row = map[string]any

// ErrNoRows is returned by FindOneInto when nothing matches (it replaces pgx.ErrNoRows).
var ErrNoRows = mongo.ErrNoDocuments

// Store holds the client and the application database. DB is used outside transactions (the old pool).
type Store struct {
	Client *mongo.Client
	DB     Q
}

// Q is the database, optionally inside a transaction (it replaces both the old store.Q and pgx.Tx).
// Every collection call made through a Q that is in a transaction joins that transaction, whatever ctx is passed.
type Q struct {
	db   *mongo.Database
	sess *mongo.Session
	now  time.Time // the transaction's now(); zero outside a transaction
	st   *txState  // shared by everything in one transaction attempt
}

// Zone is the business time zone (APP_TZ, default India). "Today", dates of timestamps and the times read back
// are in this zone; main also sets the process's local zone to it.
func Zone() string {
	if z := os.Getenv("APP_TZ"); z != "" {
		return z
	}
	return "Asia/Kolkata"
}

var (
	locMu    sync.Mutex
	locName  string
	locValue *time.Location
)

// Loc is the business zone as a *time.Location (UTC if the zone cannot be loaded).
func Loc() *time.Location {
	z := Zone()
	locMu.Lock()
	defer locMu.Unlock()
	if locValue == nil || locName != z {
		l, err := time.LoadLocation(z)
		if err != nil {
			l = time.UTC
		}
		locName, locValue = z, l
	}
	return locValue
}

// Today is current_date: today's date in the business zone, as YYYY-MM-DD.
func Today() string { return DateOf(time.Now()) }

// DateOf is timestamp::date: the date of t in the business zone, as YYYY-MM-DD.
func DateOf(t time.Time) string { return t.In(Loc()).Format("2006-01-02") }

// NewID is gen_random_uuid(): a random (version 4) UUID in the canonical lower-case form.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return string(s[:])
}

// Open connects to MongoDB (url, e.g. mongodb://localhost:27017/?replicaSet=rs0&directConnection=true) and uses
// the database dbName. Transactions need a replica set (a one-node one is enough).
func Open(ctx context.Context, url, dbName string) (*Store, error) {
	opts := options.Client().ApplyURI(url).
		SetBSONOptions(&options.BSONOptions{UseLocalTimeZone: true, DefaultDocumentMap: true}).
		SetServerSelectionTimeout(10 * time.Second)
	c, err := mongo.Connect(opts)
	if err != nil {
		return nil, err
	}
	if err := c.Ping(ctx, nil); err != nil {
		_ = c.Disconnect(context.Background())
		return nil, fmt.Errorf("database not reachable: %w", err)
	}
	return &Store{Client: c, DB: Q{db: c.Database(dbName)}}, nil
}

// Close disconnects the client.
func (s *Store) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Client.Disconnect(ctx)
}

// Tx runs fn in a multi-document transaction and commits it. A write conflict or another transient error (two
// transactions writing, or Lock-ing, the same document) aborts the attempt and runs fn again from the start, so fn
// must have no side effects outside the database (no files, HTTP calls, e-mails, or appends to outer slices that it
// does not reset). Any error fn returns aborts the transaction and is returned as is.
func (s *Store) Tx(ctx context.Context, fn func(tx Q) error) error {
	sess, err := s.Client.StartSession()
	if err != nil {
		return err
	}
	defer sess.EndSession(context.WithoutCancel(ctx))
	txo := options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority())
	_, err = sess.WithTransaction(ctx, func(context.Context) (any, error) {
		st := &txState{}
		err := fn(Q{db: s.DB.db, sess: sess, now: time.Now().Truncate(time.Millisecond), st: st})
		if err == nil {
			err = st.failed() // an operation failed (and aborted the transaction) but fn went on
		}
		return nil, err
	}, txo)
	return err
}

// C is a collection, bound to q's transaction if there is one.
func (q Q) C(name string) Coll {
	return Coll{q: q, name: name, t: tableByName[name], mc: q.db.Collection(name)}
}

// InTx reports whether q is inside a transaction.
func (q Q) InTx() bool { return q.sess != nil }

// Bind returns ctx carrying q's session (inside a transaction) or no session at all, for direct driver calls on
// Database(). Collection calls through C bind it themselves.
func (q Q) Bind(ctx context.Context) context.Context {
	if q.sess != nil {
		return mongo.NewSessionContext(ctx, q.sess)
	}
	return noSession{ctx}
}

// Database is the driver's database handle. Writes made with it bypass the store's rules: write through C.
func (q Q) Database() *mongo.Database { return q.db }

// Now is now(): inside a transaction the same instant for the whole attempt (as PostgreSQL's now() was the
// transaction's start), otherwise the current time. Millisecond precision, as stored.
func (q Q) Now() time.Time {
	if q.sess != nil && !q.now.IsZero() {
		return q.now
	}
	return time.Now().Truncate(time.Millisecond)
}

// today is current_date for q (the transaction's date inside a transaction).
func (q Q) today() string { return DateOf(q.Now()) }

// outside is q without its transaction (for sequences, which never roll back).
func (q Q) outside() Q { return Q{db: q.db} }

// noSession hides any session a context carries, so a call made with it runs outside every transaction.
type noSession struct{ context.Context }

func (c noSession) Value(key any) any {
	v := c.Context.Value(key)
	if _, ok := v.(*mongo.Session); ok {
		return nil
	}
	return v
}

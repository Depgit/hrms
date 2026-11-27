package api

import (
	"context"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// authLimiter slows down guessing (AUTH-041, AUTH-042): wrong passwords, unknown Employee IDs and wrong OTPs count as
// failures per Employee ID AND address (owner, 27 Nov 2026: a whole office signs in from one Wi-Fi address, so one
// person's mistakes must not lock everyone out): past authFailMax in authWindow that ID from that address gets 429
// until the window passes. The address as a whole has a much higher ceiling (authIPFailMax) so one machine still
// cannot try many IDs. OTP sends are counted per address too (authOTPMax in authWindow), and each Employee ID
// gets at most authOTPPerDay OTPs a day (counted from the stored OTPs, so a restart does not reset it), which caps
// the SMS cost and stops anyone flooding a person's phone. The per-address counts live in memory.
type authLimiter struct {
	mu      sync.Mutex
	fails   map[string][]time.Time // by address|ID
	otps    map[string][]time.Time // by address|ID
	ipFails map[string][]time.Time // by address
	ipOTPs  map[string][]time.Time // by address
	// otpMax and failMax are authOTPMax and authFailMax, or the Dev values with OTP_DEV_ECHO on: test scripts sign
	// many people in from one address, trying a password before activating with OTP. The per-ID daily cap still
	// applies.
	otpMax, failMax, ipFailMax, ipOTPMax int
}

const (
	authWindow     = 5 * time.Minute // owner, 27 Nov 2026: was 15
	authFailMax    = 10
	authOTPMax     = 10
	authOTPMaxDev  = 300
	authOTPPerDay  = 10
	authFailMaxDev = 300
	// The whole address (an office on one Wi-Fi): enough for many people's mistakes, not for trying IDs in bulk.
	authIPFailMax = 100
	authIPOTPMax  = 100
)

func newAuthLimiter(dev bool) *authLimiter {
	l := &authLimiter{fails: map[string][]time.Time{}, otps: map[string][]time.Time{}, ipFails: map[string][]time.Time{}, ipOTPs: map[string][]time.Time{},
		otpMax: authOTPMax, failMax: authFailMax, ipFailMax: authIPFailMax, ipOTPMax: authIPOTPMax}
	if dev {
		l.otpMax, l.failMax, l.ipFailMax, l.ipOTPMax = authOTPMaxDev, authFailMaxDev, authFailMaxDev, authOTPMaxDev
	}
	return l
}

func (l *authLimiter) recent(m map[string][]time.Time, ip string) []time.Time {
	now := time.Now()
	if len(m) > 50000 { // forget idle addresses
		for k, v := range m {
			if len(joinPrune(v, now.Add(-authWindow))) == 0 {
				delete(m, k)
			}
		}
	}
	v := joinPrune(m[ip], now.Add(-authWindow))
	m[ip] = v
	return v
}

var errTooMany = &apiError{Status: 429, Message: "Too many attempts for this Employee ID. Wait 5 minutes and try again."}
var errTooManyIP = &apiError{Status: 429, Message: "Too many attempts from this network. Wait 5 minutes and try again."}

func limitKey(ip, id string) string { return ip + "|" + id }

// blocked: this ID from this address, or the address as a whole, is past its failures; no password or OTP now.
func (l *authLimiter) blocked(ip, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.recent(l.ipFails, ip)) >= l.ipFailMax {
		return errTooManyIP
	}
	if len(l.recent(l.fails, limitKey(ip, id))) >= l.failMax {
		return errTooMany
	}
	return nil
}

// failed records a wrong password, an unknown ID or a wrong OTP for id from ip.
func (l *authLimiter) failed(ip, id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	k := limitKey(ip, id)
	l.fails[k] = append(l.recent(l.fails, k), now)
	l.ipFails[ip] = append(l.recent(l.ipFails, ip), now)
}

// otpSend records an OTP request for id from ip and says whether it may go ahead.
func (l *authLimiter) otpSend(ip, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.recent(l.ipOTPs, ip)) >= l.ipOTPMax {
		return errTooManyIP
	}
	k := limitKey(ip, id)
	if len(l.recent(l.otps, k)) >= l.otpMax {
		return errTooMany
	}
	now := time.Now()
	l.otps[k] = append(l.otps[k], now)
	l.ipOTPs[ip] = append(l.ipOTPs[ip], now)
	return nil
}

// otpDailyCap refuses a new OTP for an Employee ID that already had authOTPPerDay in the last 24 hours.
func (s *Server) otpDailyCap(ctx context.Context, id string) error {
	n, err := s.st.DB.C("auth_otp").Count(ctx, bson.M{"employee_id": id, "sent_at": bson.M{"$gt": time.Now().Add(-24 * time.Hour)}})
	if err != nil {
		return err
	}
	if n >= authOTPPerDay {
		return &apiError{Status: 429, Message: "Too many OTPs for this Employee ID today. Try again tomorrow, or ask HR."}
	}
	return nil
}

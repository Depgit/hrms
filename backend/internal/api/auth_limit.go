package api

import (
	"context"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// authLimiter slows down guessing from one address (AUTH-041, AUTH-042): wrong passwords, unknown Employee IDs and
// wrong OTPs count as failures, and past authFailMax in authWindow the address gets 429 on sign-in and OTP checks
// until the window passes. OTP sends are counted per address too (authOTPMax in authWindow), and each Employee ID
// gets at most authOTPPerDay OTPs a day (counted from the stored OTPs, so a restart does not reset it), which caps
// the SMS cost and stops anyone flooding a person's phone. The per-address counts live in memory.
type authLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
	otps  map[string][]time.Time
	// otpMax and failMax are authOTPMax and authFailMax, or the Dev values with OTP_DEV_ECHO on: test scripts sign
	// many people in from one address, trying a password before activating with OTP. The per-ID daily cap still
	// applies.
	otpMax, failMax int
}

const (
	authWindow     = 1 * time.Minute // owner, 27 Nov 2026: was 15
	authFailMax    = 10
	authOTPMax     = 10
	authOTPMaxDev  = 300
	authOTPPerDay  = 10
	authFailMaxDev = 300
)

func newAuthLimiter(dev bool) *authLimiter {
	l := &authLimiter{fails: map[string][]time.Time{}, otps: map[string][]time.Time{}, otpMax: authOTPMax, failMax: authFailMax}
	if dev {
		l.otpMax, l.failMax = authOTPMaxDev, authFailMaxDev
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

var errTooMany = &apiError{Status: 429, Message: "Too many attempts from this device. Wait 1 minute and try again."}

// blocked: the address is past its failures; it may not try a password or an OTP now.
func (l *authLimiter) blocked(ip string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.recent(l.fails, ip)) >= l.failMax {
		return errTooMany
	}
	return nil
}

// failed records a wrong password, an unknown ID or a wrong OTP from ip.
func (l *authLimiter) failed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip] = append(l.recent(l.fails, ip), time.Now())
}

// otpSend records an OTP request from ip and says whether it may go ahead.
func (l *authLimiter) otpSend(ip string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.recent(l.otps, ip)) >= l.otpMax {
		return errTooMany
	}
	l.otps[ip] = append(l.otps[ip], time.Now())
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

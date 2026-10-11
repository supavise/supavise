package advisors

import "fmt"

// AuthSettings are the settings of a project's Auth service the lints computed outside SQL read.
// internal/api fills it from the project's saved settings and defaults.
type AuthSettings struct {
	// EmailEnabled is external_email_enabled.
	EmailEnabled bool
	// MailerOTPExp is mailer_otp_exp, in seconds.
	MailerOTPExp int64
	// PasswordHIBPEnabled is password_hibp_enabled: leaked password protection.
	PasswordHIBPEnabled bool
	// MFA methods that can both enroll and verify factors.
	TOTPEnabled, PhoneEnabled, WebAuthnEnabled bool
	// DBMaxPoolSize and DBMaxPoolSizeUnit ("connections" or "percent") are db_max_pool_size and
	// db_max_pool_size_unit.
	DBMaxPoolSize     int64
	DBMaxPoolSizeUnit string
}

// OTPExpiryLimit is the longest email OTP expiry, in seconds, that does not raise
// auth_otp_long_expiry: the hour hosted's going-into-production checklist recommends.
const OTPExpiryLimit = 3600

// ConfigLints returns the lints hosted computes from a project's configuration and not from its
// schema, for the ones Supavise can: the Auth settings. Names, levels, categories, texts and
// links are the ones hosted's Management API returns for the same findings (published advisor
// output of hosted projects, and the recorded responses of the Supabase CLI's end-to-end suite).
//
// Not computed, and why:
//   - ssl_not_enforced, network_restrictions_not_set, pitr_not_enabled, password_requirements_min_length,
//     auth_password_policy_missing, no_backup_admin and leaked_service_key: the response specs type
//     these as metadata.type "compliance", and the published advisor output of ordinary hosted projects
//     does not list them, so they belong to hosted's compliance add-ons. Supavise has no add-on to
//     key them on, and firing them on every project would add findings hosted does not show.
//   - auth_otp_short_length: Auth refuses an OTP shorter than 6 digits, the length hosted
//     asks for, so the finding cannot arise.
//   - auth_allow_anonymous_sign_ins: the SQL Studio redistributes does not carry it, and its
//     trigger and texts live in a repository without a license.
//   - vulnerable_postgres_version: needs a feed of security releases per Postgres version.
func ConfigLints(a AuthSettings) []Lint {
	var out []Lint
	if !a.PasswordHIBPEnabled {
		out = append(out, build(fields{
			Name:        "auth_leaked_password_protection",
			Title:       "Leaked Password Protection Disabled",
			Level:       LevelWarn,
			Categories:  []string{CategorySecurity},
			Description: "Leaked password protection is currently disabled.",
			Detail:      "Supabase Auth prevents the use of compromised passwords by checking against HaveIBeenPwned.org. Enable this feature to enhance security.",
			CacheKey:    "auth_leaked_password_protection",
			Remediation: DocsURL + "/guides/auth/password-security#password-strength-and-leaked-password-protection",
			Metadata:    authMetadata(),
		}))
	}
	if a.mfaMethods() < 2 {
		out = append(out, build(fields{
			Name:        "auth_insufficient_mfa_options",
			Title:       "Insufficient MFA Options",
			Level:       LevelWarn,
			Categories:  []string{CategorySecurity},
			Description: "This project has too few multi-factor authentication (MFA) options enabled.",
			Detail:      "Your project has too few MFA options enabled, which may weaken account security. Enable more MFA methods to enhance security.",
			CacheKey:    "auth_insufficient_mfa_options",
			Remediation: DocsURL + "/guides/auth/auth-mfa",
			Metadata:    authMetadata(),
		}))
	}
	if a.EmailEnabled && a.MailerOTPExp > OTPExpiryLimit {
		out = append(out, build(fields{
			Name:        "auth_otp_long_expiry",
			Title:       "Auth OTP long expiry",
			Level:       LevelWarn,
			Categories:  []string{CategorySecurity},
			Description: "OTP expiry exceeds recommended threshold",
			Detail:      "We have detected that you have enabled the email provider with the OTP expiry set to more than an hour. It is recommended to set this value to less than an hour.",
			CacheKey:    "auth_otp_long_expiry",
			Remediation: DocsURL + "/guides/platform/going-into-prod#security",
			Metadata:    authMetadata(),
		}))
	}
	if a.DBMaxPoolSizeUnit != "percent" {
		out = append(out, build(fields{
			Name:        "auth_db_connections_absolute",
			Title:       "Auth DB Connection Strategy is not Percentage",
			Level:       LevelInfo,
			Categories:  []string{CategoryPerformance},
			Description: "Using a percentage based allocation connection strategy for Auth can help to improve the server's performance when increasing instance size.",
			Detail:      fmt.Sprintf("Your project's Auth server is configured to use at most %d connections. Increasing the instance size without manually adjusting this number will not improve the performance of the Auth server. Switch to a percentage based connection allocation strategy instead.", a.DBMaxPoolSize),
			CacheKey:    "auth_db_connections_absolute",
			Remediation: DocsURL + "/guides/deployment/going-into-prod",
			Metadata:    authMetadata(),
		}))
	}
	return out
}

func authMetadata() map[string]any { return map[string]any{"type": "auth", "entity": "Auth"} }

// mfaMethods counts the MFA methods users can enroll and verify.
func (a AuthSettings) mfaMethods() int {
	n := 0
	for _, on := range []bool{a.TOTPEnabled, a.PhoneEnabled, a.WebAuthnEnabled} {
		if on {
			n++
		}
	}
	return n
}

// configNames are the lints ConfigLints can return.
var configNames = map[string]bool{
	"auth_leaked_password_protection": true,
	"auth_insufficient_mfa_options":   true,
	"auth_otp_long_expiry":            true,
	"auth_db_connections_absolute":    true,
}

// IsConfigLint reports whether name is a lint ConfigLints computes.
func IsConfigLint(name string) bool { return configNames[name] }

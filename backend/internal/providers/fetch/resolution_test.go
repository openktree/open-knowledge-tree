package fetch

import "testing"

// TestIsJSBoilerplate covers every phrase in jsBoilerplatePhrases
// so a regression (a phrase accidentally dropped during a refactor,
// or a new phrase that doesn't actually match the corpus) is caught
// at the unit level. Each case uses a snippet drawn from the real
// corpus scripts/diagnose-sources surfaced, so the test doubles as
// documentation of what each phrase is defending against.
func TestIsJSBoilerplate(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		// Original <noscript> fallbacks.
		{"javascript is disabled", "JavaScript is disabled in your browser.", true},
		{"please enable javascript", "Please enable JavaScript to continue.", true},
		{"enable it to continue", "You must enable it to continue.", true},
		{"doesn't work properly without javascript", "This site doesn't work properly without JavaScript enabled.", true},
		{"you need to enable javascript", "You need to enable JavaScript to view this page.", true},

		// OUP "Validate User" captcha interstitial.
		{"oup validate user title", "Validate User\nWe are sorry, but we are experiencing unusual traffic at this time.", true},
		{"oup could not validate captcha", "Could not validate captcha. Please try again. Take me to my Content.", true},
		{"oup experiencing unusual traffic", "We are sorry, but we are experiencing unusual traffic at this time. Please help us confirm that you are not a robot.", true},

		// Wiley "Cookies disabled" gate.
		{"wiley cookies are disabled", "Cookies disabled Cookies are disabled for this browser. Wiley Online Library requires cookies for authentication.", true},
		{"wiley requires cookies for authentication", "Wiley Online Library requires cookies for authentication and use of other site features.", true},

		// Generic JS-bot-challenge phrases.
		{"making sure you're not a bot", "Making sure you're not a bot! Loading... Please wait a moment.", true},
		{"please verify you are a human", "Please verify you are a human to continue.", true},
		{"site protection verifying your request", "Site Protection: Verifying your Request This page is unavailable.", true},

		// "Dear visitor" captcha landing.
		{"dear visitor captcha", "Dear visitor To continue browsing and help us fight cybercrime, please solve the CAPTCHA you see below.", true},
		{"fight cybercrime", "Help us fight cybercrime by solving the CAPTCHA below.", true},

		// Cloudflare 5xx landing.
		{"connection timed out error code 522", "Connection timed out Error code 522 The initial connection between Cloudflare's network and the origin web server timed out.", true},

		// Broken redirect.
		{"the page isn't redirecting properly", "The page isn't redirecting properly Camoufox has detected that the server is redirecting the request in a way that will never complete.", true},

		// Negative cases — real article snippets that must NOT match.
		{"empty", "", false},
		{"real article body", "This study examines the role of mycorrhizal symbiosis in phosphorus efficiency across tropical agroforestry systems. The findings suggest that inoculation with compatible fungi can improve nutrient uptake.", false},
		{"article mentioning cookies in passing", "We use cookies to analyze traffic. By continuing you agree to our use of cookies. This paper presents a novel method for leaf litter decomposition analysis.", false},
		{"article with the word validate", "We validate the model against a held-out test set and report an F1 score of 0.87. The user study confirms the system is usable in production.", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsJSBoilerplate(tc.text)
			if got != tc.want {
				t.Errorf("IsJSBoilerplate(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestIsHTMLLeakBoilerplate covers the PerimeterX / GTM
// iframe-leak signature. The positive case is drawn from the
// real jstor.org corpus; the negative cases confirm a real
// article whose body merely contains an <iframe> later on is
// not flagged.
func TestIsHTMLLeakBoilerplate(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "jstor perimeterx iframe leak",
			text: `<iframe title="Google Tag Manager" src="https://www.googletagmanager.com/ns.html?id=GTM-N6GDC22" height="0" width="0" style="display:none;visibility:hidden"></iframe> <div><img id="px-pixel-nojs" aria-hidden="true" src="/u4K0s8nX/xhr/api/v1/collector`,
			want: true,
		},
		{
			name: "iframe single quotes variant",
			text: `<iframe title='Google Tag Manager' src='https://www.googletagmanager.com/ns.html?id=GTM-N6GDC22'></iframe>`,
			want: true,
		},
		{
			name: "empty",
			text: "",
			want: false,
		},
		{
			name: "real article that mentions iframe later",
			text: "This paper presents a video-based study. The supplementary material includes an <iframe> embedding the demo reel on the project page. We analyze the results across four conditions.",
			want: false,
		},
		{
			name: "real article body no html",
			text: "Mycorrhizal symbiosis in managing phosphorus efficiency in tropical agroforestry. The field study was conducted across three sites over two growing seasons.",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsHTMLLeakBoilerplate(tc.text)
			if got != tc.want {
				t.Errorf("IsHTMLLeakBoilerplate(%q) = %v, want %v", firstChars(tc.text, 80), got, tc.want)
			}
		})
	}
}

// firstChars returns the first n characters of s, for readable
// test failure output on long strings.
func firstChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
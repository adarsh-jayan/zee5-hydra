/*
 * Copyright © 2015-2018 Aeneas Rekkas <aeneas+oss@aeneas.io>
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * @author		Aeneas Rekkas <aeneas+oss@aeneas.io>
 * @copyright 	2015-2018 Aeneas Rekkas <aeneas+oss@aeneas.io>
 * @license 	Apache-2.0
 */

package oauth2

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"reflect"
	"strings"
	"time"

	"go.step.sm/crypto/jose"

	"github.com/ory/x/httprouterx"

	"github.com/pborman/uuid"

	"github.com/ory/x/errorsx"

	"github.com/julienschmidt/httprouter"
	"github.com/pkg/errors"

	jwt2 "github.com/ory/fosite/token/jwt"

	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/openid"
	"github.com/ory/fosite/token/jwt"
	"github.com/ory/x/urlx"

	"github.com/ory/hydra/client"
	"github.com/ory/hydra/consent"
	"github.com/ory/hydra/driver/config"
	"github.com/ory/hydra/x"
)

const (
	DefaultLoginPath      = "/oauth2/fallbacks/login"
	DefaultConsentPath    = "/oauth2/fallbacks/consent"
	DefaultPostLogoutPath = "/oauth2/fallbacks/logout/callback"
	DefaultLogoutPath     = "/oauth2/fallbacks/logout"
	DefaultErrorPath      = "/oauth2/fallbacks/error"
	TokenPath             = "/oauth2/token" // #nosec G101
	AuthPath              = "/oauth2/auth"
	LogoutPath            = "/oauth2/sessions/logout"

	UserinfoPath  = "/userinfo"
	WellKnownPath = "/.well-known/openid-configuration"
	JWKPath       = "/.well-known/jwks.json"

	// IntrospectPath points to the OAuth2 introspection endpoint.
	IntrospectPath   = "/oauth2/introspect"
	RevocationPath   = "/oauth2/revoke"
	DeleteTokensPath = "/oauth2/tokens" // #nosec G101
)

type Handler struct {
	r InternalRegistry
	c *config.DefaultProvider
}

func NewHandler(r InternalRegistry, c *config.DefaultProvider) *Handler {
	return &Handler{r: r, c: c}
}

func (h *Handler) SetRoutes(admin *httprouterx.RouterAdmin, public *httprouterx.RouterPublic, corsMiddleware func(http.Handler) http.Handler) {
	public.Handler("OPTIONS", TokenPath, corsMiddleware(http.HandlerFunc(h.handleOptions)))
	public.Handler("POST", TokenPath, corsMiddleware(http.HandlerFunc(h.performOAuth2TokenFlow)))

	public.GET(AuthPath, h.performOAuth2AuthorizationFlow)
	public.POST(AuthPath, h.performOAuth2AuthorizationFlow)
	public.GET(LogoutPath, h.performOidcFrontOrBackChannelLogout)
	public.POST(LogoutPath, h.performOidcFrontOrBackChannelLogout)

	public.GET(DefaultLoginPath, h.fallbackHandler("", "", http.StatusOK, config.KeyLoginURL))
	public.GET(DefaultConsentPath, h.fallbackHandler("", "", http.StatusOK, config.KeyConsentURL))
	public.GET(DefaultLogoutPath, h.fallbackHandler("", "", http.StatusOK, config.KeyLogoutURL))
	public.GET(DefaultPostLogoutPath, h.fallbackHandler(
		"You logged out successfully!",
		"The Default Post Logout URL is not set which is why you are seeing this fallback page. Your log out request however succeeded.",
		http.StatusOK,
		config.KeyLogoutRedirectURL,
	))
	public.GET(DefaultErrorPath, h.DefaultErrorHandler)

	public.Handler("OPTIONS", RevocationPath, corsMiddleware(http.HandlerFunc(h.handleOptions)))
	public.Handler("POST", RevocationPath, corsMiddleware(http.HandlerFunc(h.revokeOAuth2Token)))
	public.Handler("OPTIONS", WellKnownPath, corsMiddleware(http.HandlerFunc(h.handleOptions)))
	public.Handler("GET", WellKnownPath, corsMiddleware(http.HandlerFunc(h.discoverOidcConfiguration)))
	public.Handler("OPTIONS", UserinfoPath, corsMiddleware(http.HandlerFunc(h.handleOptions)))
	public.Handler("GET", UserinfoPath, corsMiddleware(http.HandlerFunc(h.getOidcUserInfo)))
	public.Handler("POST", UserinfoPath, corsMiddleware(http.HandlerFunc(h.getOidcUserInfo)))

	admin.POST(IntrospectPath, h.adminIntrospectOAuth2Token)
	admin.DELETE(DeleteTokensPath, h.adminDeleteOAuth2Token)
}

// swagger:route GET /oauth2/sessions/logout v0alpha2 performOidcFrontOrBackChannelLogout
//
// OpenID Connect Front- or Back-channel Enabled Logout
//
// This endpoint initiates and completes user logout at Ory Hydra and initiates OpenID Connect Front- / Back-channel logout:
//
// - https://openid.net/specs/openid-connect-frontchannel-1_0.html
// - https://openid.net/specs/openid-connect-backchannel-1_0.html
//
// Back-channel logout is performed asynchronously and does not affect logout flow.
//
//     Schemes: http, https
//
//     Responses:
//       302: emptyResponse
func (h *Handler) performOidcFrontOrBackChannelLogout(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	ctx := r.Context()
	handled, err := h.r.ConsentStrategy().HandleOpenIDConnectLogout(ctx, w, r)

	if errors.Is(err, consent.ErrAbortOAuth2Request) {
		return
	} else if err != nil {
		x.LogError(r, err, h.r.Logger())
		h.forwardError(w, r, err)
		return
	}

	if len(handled.FrontChannelLogoutURLs) == 0 {
		http.Redirect(w, r, handled.RedirectTo, http.StatusFound)
		return
	}

	// TODO How are we supposed to test this? Maybe with cypress? #1368
	t, err := template.New("logout").Parse(`<html>
<head>
    <meta http-equiv="refresh" content="7; URL={{ .RedirectTo }}">
</head>
<style type="text/css">
    iframe { position: absolute; left: 0; top: 0; height: 0; width: 0; border: none; }
</style>
<script>
    var total = {{ len .FrontChannelLogoutURLs }};
    var redir = {{ .RedirectTo }};

	function redirect() {
		window.location.replace(redir);

		// In case replace failed try href
		setTimeout(function () {
			window.location.href = redir;
		}, 250); // Show message after http-equiv="refresh"
	}

    function done() {
        total--;
        if (total < 1) {
			setTimeout(redirect, 500);
        }
    }

	setTimeout(redirect, 7000); // redirect after 5 seconds if e.g. an iframe doesn't load

	// If the redirect takes unusually long, show a message
	setTimeout(function () {
		document.getElementById("redir").style.display = "block";
	}, 2000);
</script>
<body>
<noscript>
    <p>
        JavaScript is disabled - you should be redirected in 5 seconds but if not, click <a
            href="{{ .RedirectTo }}">here</a> to continue.
    </p>
</noscript>

<p id="redir" style="display: none">
    Redirection takes unusually long. If you are not being redirected within the next seconds, click <a href="{{ .RedirectTo }}">here</a> to continue.
</p>

{{ range .FrontChannelLogoutURLs }}<iframe src="{{ . }}" onload="done(this)"></iframe>
{{ end }}
</body>
</html>`)
	if err != nil {
		x.LogError(r, err, h.r.Logger())
		h.forwardError(w, r, err)
		return
	}

	if err := t.Execute(w, handled); err != nil {
		x.LogError(r, err, h.r.Logger())
		h.forwardError(w, r, err)
		return
	}
}

// OpenID Connect Discovery ;etadata
//
// It includes links to several endpoints (for example `/oauth2/token`) and exposes information on supported signature algorithms
// among others.
//
// swagger:model oidcConfiguration
type OIDCConfiguration struct {
	// URL using the https scheme with no query or fragment component that the OP asserts as its IssuerURL Identifier.
	// If IssuerURL discovery is supported , this value MUST be identical to the issuer value returned
	// by WebFinger. This also MUST be identical to the iss Claim value in ID Tokens issued from this IssuerURL.
	//
	// required: true
	// example: https://playground.ory.sh/ory-hydra/public/
	Issuer string `json:"issuer"`

	// URL of the OP's OAuth 2.0 Authorization Endpoint.
	//
	// required: true
	// example: https://playground.ory.sh/ory-hydra/public/oauth2/auth
	AuthURL string `json:"authorization_endpoint"`

	// URL of the OP's Dynamic Client Registration Endpoint.
	// example: https://playground.ory.sh/ory-hydra/admin/client
	RegistrationEndpoint string `json:"registration_endpoint,omitempty"`

	// URL of the OP's OAuth 2.0 Token Endpoint
	//
	// required: true
	// example: https://playground.ory.sh/ory-hydra/public/oauth2/token
	TokenURL string `json:"token_endpoint"`

	// URL of the OP's JSON Web Key Set [JWK] document. This contains the signing key(s) the RP uses to validate
	// signatures from the OP. The JWK Set MAY also contain the Server's encryption key(s), which are used by RPs
	// to encrypt requests to the Server. When both signing and encryption keys are made available, a use (Key Use)
	// parameter value is REQUIRED for all keys in the referenced JWK Set to indicate each key's intended usage.
	// Although some algorithms allow the same key to be used for both signatures and encryption, doing so is
	// NOT RECOMMENDED, as it is less secure. The JWK x5c parameter MAY be used to provide X.509 representations of
	// keys provided. When used, the bare key values MUST still be present and MUST match those in the certificate.
	//
	// required: true
	// example: https://playground.ory.sh/ory-hydra/public/.well-known/jwks.json
	JWKsURI string `json:"jwks_uri"`

	// JSON array containing a list of the Subject Identifier types that this OP supports. Valid types include
	// pairwise and public.
	//
	// required: true
	// example:
	//   - public
	//   - pairwise
	SubjectTypes []string `json:"subject_types_supported"`

	// JSON array containing a list of the OAuth 2.0 response_type values that this OP supports. Dynamic OpenID
	// Providers MUST support the code, id_token, and the token id_token Response Type values.
	//
	// required: true
	ResponseTypes []string `json:"response_types_supported"`

	// JSON array containing a list of the Claim Names of the Claims that the OpenID Provider MAY be able to supply
	// values for. Note that for privacy or other reasons, this might not be an exhaustive list.
	ClaimsSupported []string `json:"claims_supported"`

	// JSON array containing a list of the OAuth 2.0 Grant Type values that this OP supports.
	GrantTypesSupported []string `json:"grant_types_supported"`

	// JSON array containing a list of the OAuth 2.0 response_mode values that this OP supports.
	ResponseModesSupported []string `json:"response_modes_supported"`

	// URL of the OP's UserInfo Endpoint.
	UserinfoEndpoint string `json:"userinfo_endpoint"`

	// SON array containing a list of the OAuth 2.0 [RFC6749] scope values that this server supports. The server MUST
	// support the openid scope value. Servers MAY choose not to advertise some supported scope values even when this parameter is used
	ScopesSupported []string `json:"scopes_supported"`

	// JSON array containing a list of Client Authentication methods supported by this Token Endpoint. The options are
	// client_secret_post, client_secret_basic, client_secret_jwt, and private_key_jwt, as described in Section 9 of OpenID Connect Core 1.0
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`

	// 	JSON array containing a list of the JWS [JWS] signing algorithms (alg values) [JWA] supported by the UserInfo Endpoint to encode the Claims in a JWT [JWT].
	UserinfoSigningAlgValuesSupported []string `json:"userinfo_signing_alg_values_supported"`

	// JSON array containing a list of the JWS signing algorithms (alg values) supported by the OP for the ID Token
	// to encode the Claims in a JWT.
	//
	// required: true
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`

	// Algorithm used to sign OpenID Connect ID Tokens.
	//
	// required: true
	IDTokenSignedResponseAlg []string `json:"id_token_signed_response_alg"`

	// Algorithm used to sign OpenID Connect Userinfo Responses.
	//
	// required: true
	UserinfoSignedResponseAlg []string `json:"userinfo_signed_response_alg"`

	// 	Boolean value specifying whether the OP supports use of the request parameter, with true indicating support.
	RequestParameterSupported bool `json:"request_parameter_supported"`

	// Boolean value specifying whether the OP supports use of the request_uri parameter, with true indicating support.
	RequestURIParameterSupported bool `json:"request_uri_parameter_supported"`

	// Boolean value specifying whether the OP requires any request_uri values used to be pre-registered
	// using the request_uris registration parameter.
	RequireRequestURIRegistration bool `json:"require_request_uri_registration"`

	// Boolean value specifying whether the OP supports use of the claims parameter, with true indicating support.
	ClaimsParameterSupported bool `json:"claims_parameter_supported"`

	// URL of the authorization server's OAuth 2.0 revocation endpoint.
	RevocationEndpoint string `json:"revocation_endpoint"`

	// Boolean value specifying whether the OP supports back-channel logout, with true indicating support.
	BackChannelLogoutSupported bool `json:"backchannel_logout_supported"`

	// Boolean value specifying whether the OP can pass a sid (session ID) Claim in the Logout Token to identify the RP
	// session with the OP. If supported, the sid Claim is also included in ID Tokens issued by the OP
	BackChannelLogoutSessionSupported bool `json:"backchannel_logout_session_supported"`

	// Boolean value specifying whether the OP supports HTTP-based logout, with true indicating support.
	FrontChannelLogoutSupported bool `json:"frontchannel_logout_supported"`

	// Boolean value specifying whether the OP can pass iss (issuer) and sid (session ID) query parameters to identify
	// the RP session with the OP when the frontchannel_logout_uri is used. If supported, the sid Claim is also
	// included in ID Tokens issued by the OP.
	FrontChannelLogoutSessionSupported bool `json:"frontchannel_logout_session_supported"`

	// URL at the OP to which an RP can perform a redirect to request that the End-User be logged out at the OP.
	EndSessionEndpoint string `json:"end_session_endpoint"`

	// JSON array containing a list of the JWS signing algorithms (alg values) supported by the OP for Request Objects,
	// which are described in Section 6.1 of OpenID Connect Core 1.0 [OpenID.Core]. These algorithms are used both when
	// the Request Object is passed by value (using the request parameter) and when it is passed by reference
	// (using the request_uri parameter).
	RequestObjectSigningAlgValuesSupported []string `json:"request_object_signing_alg_values_supported"`

	// JSON array containing a list of Proof Key for Code Exchange (PKCE) [RFC7636] code challenge methods supported
	// by this authorization server.
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

// swagger:route GET /.well-known/openid-configuration v0alpha2 discoverOidcConfiguration
//
// OpenID Connect Discovery
//
// The well known endpoint an be used to retrieve information for OpenID Connect clients. We encourage you to not roll
// your own OpenID Connect client but to use an OpenID Connect client library instead. You can learn more on this
// flow at https://openid.net/specs/openid-connect-discovery-1_0.html .
//
// Popular libraries for OpenID Connect clients include oidc-client-js (JavaScript), go-oidc (Golang), and others.
// For a full list of clients go here: https://openid.net/developers/certified/
//
//     Produces:
//     - application/json
//
//     Schemes: http, https
//
//     Responses:
//       200: oidcConfiguration
//       default: oAuth2ApiError
func (h *Handler) discoverOidcConfiguration(w http.ResponseWriter, r *http.Request) {
	key, err := h.r.OpenIDJWTStrategy().GetPublicKey(r.Context())
	if err != nil {
		h.r.Writer().WriteError(w, r, err)
		return
	}
	h.r.Writer().Write(w, r, &OIDCConfiguration{
		Issuer:                                 h.c.IssuerURL(r.Context()).String(),
		AuthURL:                                h.c.OAuth2AuthURL(r.Context()).String(),
		TokenURL:                               h.c.OAuth2TokenURL(r.Context()).String(),
		JWKsURI:                                h.c.JWKSURL(r.Context()).String(),
		RevocationEndpoint:                     urlx.AppendPaths(h.c.IssuerURL(r.Context()), RevocationPath).String(),
		RegistrationEndpoint:                   h.c.OAuth2ClientRegistrationURL(r.Context()).String(),
		SubjectTypes:                           h.c.SubjectTypesSupported(r.Context()),
		ResponseTypes:                          []string{"code", "code id_token", "id_token", "token id_token", "token", "token id_token code"},
		ClaimsSupported:                        h.c.OIDCDiscoverySupportedClaims(r.Context()),
		ScopesSupported:                        h.c.OIDCDiscoverySupportedScope(r.Context()),
		UserinfoEndpoint:                       h.c.OIDCDiscoveryUserinfoEndpoint(r.Context()).String(),
		TokenEndpointAuthMethodsSupported:      []string{"client_secret_post", "client_secret_basic", "private_key_jwt", "none"},
		IDTokenSigningAlgValuesSupported:       []string{key.Algorithm},
		IDTokenSignedResponseAlg:               []string{key.Algorithm},
		UserinfoSignedResponseAlg:              []string{key.Algorithm},
		GrantTypesSupported:                    []string{"authorization_code", "implicit", "client_credentials", "refresh_token"},
		ResponseModesSupported:                 []string{"query", "fragment"},
		UserinfoSigningAlgValuesSupported:      []string{"none", key.Algorithm},
		RequestParameterSupported:              true,
		RequestURIParameterSupported:           true,
		RequireRequestURIRegistration:          true,
		BackChannelLogoutSupported:             true,
		BackChannelLogoutSessionSupported:      true,
		FrontChannelLogoutSupported:            true,
		FrontChannelLogoutSessionSupported:     true,
		EndSessionEndpoint:                     urlx.AppendPaths(h.c.IssuerURL(r.Context()), LogoutPath).String(),
		RequestObjectSigningAlgValuesSupported: []string{"none", string(jose.RS256), string(jose.ES256)},
		CodeChallengeMethodsSupported:          []string{"plain", "S256"},
	})
}

// The userinfo response
// swagger:model oidcUserInfo
type oidcUserInfo struct {
	// Subject - Identifier for the End-User at the IssuerURL.
	Subject string `json:"sub"`

	// End-User's full name in displayable form including all name parts, possibly including titles and suffixes, ordered according to the End-User's locale and preferences.
	Name string `json:"name,omitempty"`

	// Given name(s) or first name(s) of the End-User. Note that in some cultures, people can have multiple given names; all can be present, with the names being separated by space characters.
	GivenName string `json:"given_name,omitempty"`

	// Surname(s) or last name(s) of the End-User. Note that in some cultures, people can have multiple family names or no family name; all can be present, with the names being separated by space characters.
	FamilyName string `json:"family_name,omitempty"`

	// Middle name(s) of the End-User. Note that in some cultures, people can have multiple middle names; all can be present, with the names being separated by space characters. Also note that in some cultures, middle names are not used.
	MiddleName string `json:"middle_name,omitempty"`

	// Casual name of the End-User that may or may not be the same as the given_name. For instance, a nickname value of Mike might be returned alongside a given_name value of Michael.
	Nickname string `json:"nickname,omitempty"`

	// Non-unique shorthand name by which the End-User wishes to be referred to at the RP, such as janedoe or j.doe. This value MAY be any valid JSON string including special characters such as @, /, or whitespace.
	PreferredUsername string `json:"preferred_username,omitempty"`

	// URL of the End-User's profile page. The contents of this Web page SHOULD be about the End-User.
	Profile string `json:"profile,omitempty"`

	// URL of the End-User's profile picture. This URL MUST refer to an image file (for example, a PNG, JPEG, or GIF image file), rather than to a Web page containing an image. Note that this URL SHOULD specifically reference a profile photo of the End-User suitable for displaying when describing the End-User, rather than an arbitrary photo taken by the End-User.
	Picture string `json:"picture,omitempty"`

	// URL of the End-User's Web page or blog. This Web page SHOULD contain information published by the End-User or an organization that the End-User is affiliated with.
	Website string `json:"website,omitempty"`

	// End-User's preferred e-mail address. Its value MUST conform to the RFC 5322 [RFC5322] addr-spec syntax. The RP MUST NOT rely upon this value being unique, as discussed in Section 5.7.
	Email string `json:"email,omitempty"`

	// True if the End-User's e-mail address has been verified; otherwise false. When this Claim Value is true, this means that the OP took affirmative steps to ensure that this e-mail address was controlled by the End-User at the time the verification was performed. The means by which an e-mail address is verified is context-specific, and dependent upon the trust framework or contractual agreements within which the parties are operating.
	EmailVerified bool `json:"email_verified,omitempty"`

	// End-User's gender. Values defined by this specification are female and male. Other values MAY be used when neither of the defined values are applicable.
	Gender string `json:"gender,omitempty"`

	// End-User's birthday, represented as an ISO 8601:2004 [ISO8601‑2004] YYYY-MM-DD format. The year MAY be 0000, indicating that it is omitted. To represent only the year, YYYY format is allowed. Note that depending on the underlying platform's date related function, providing just year can result in varying month and day, so the implementers need to take this factor into account to correctly process the dates.
	Birthdate string `json:"birthdate,omitempty"`

	// String from zoneinfo [zoneinfo] time zone database representing the End-User's time zone. For example, Europe/Paris or America/Los_Angeles.
	Zoneinfo string `json:"zoneinfo,omitempty"`

	// End-User's locale, represented as a BCP47 [RFC5646] language tag. This is typically an ISO 639-1 Alpha-2 [ISO639‑1] language code in lowercase and an ISO 3166-1 Alpha-2 [ISO3166‑1] country code in uppercase, separated by a dash. For example, en-US or fr-CA. As a compatibility note, some implementations have used an underscore as the separator rather than a dash, for example, en_US; Relying Parties MAY choose to accept this locale syntax as well.
	Locale string `json:"locale,omitempty"`

	// End-User's preferred telephone number. E.164 [E.164] is RECOMMENDED as the format of this Claim, for example, +1 (425) 555-1212 or +56 (2) 687 2400. If the phone number contains an extension, it is RECOMMENDED that the extension be represented using the RFC 3966 [RFC3966] extension syntax, for example, +1 (604) 555-1234;ext=5678.
	PhoneNumber string `json:"phone_number,omitempty"`

	// True if the End-User's phone number has been verified; otherwise false. When this Claim Value is true, this means that the OP took affirmative steps to ensure that this phone number was controlled by the End-User at the time the verification was performed. The means by which a phone number is verified is context-specific, and dependent upon the trust framework or contractual agreements within which the parties are operating. When true, the phone_number Claim MUST be in E.164 format and any extensions MUST be represented in RFC 3966 format.
	PhoneNumberVerified bool `json:"phone_number_verified,omitempty"`

	// Time the End-User's information was last updated. Its value is a JSON number representing the number of seconds from 1970-01-01T0:0:0Z as measured in UTC until the date/time.
	UpdatedAt int `json:"updated_at,omitempty"`
}

// swagger:route GET /userinfo v0alpha2 getOidcUserInfo
//
// OpenID Connect Userinfo
//
// This endpoint returns the payload of the ID Token, including the idTokenExtra values, of
// the provided OAuth 2.0 Access Token.
//
// For more information please [refer to the spec](http://openid.net/specs/openid-connect-core-1_0.html#UserInfo).
//
// In the case of authentication error, a WWW-Authenticate header might be set in the response
// with more information about the error. See [the spec](https://datatracker.ietf.org/doc/html/rfc6750#section-3)
// for more details about header format.
//
//     Produces:
//     - application/json
//
//     Schemes: http, https
//
//     Security:
//       oauth2:
//
//     Responses:
//       200: oidcUserInfo
//       default: oAuth2ApiError
func (h *Handler) getOidcUserInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	session := NewSessionWithCustomClaims("", h.c.AllowedTopLevelClaims(ctx))
	tokenType, ar, err := h.r.OAuth2Provider().IntrospectToken(ctx, fosite.AccessTokenFromRequest(r), fosite.AccessToken, session)
	if err != nil {
		rfcerr := fosite.ErrorToRFC6749Error(err)
		if rfcerr.StatusCode() == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="%s",error_description="%s"`, rfcerr.ErrorField, rfcerr.GetDescription()))
		}
		h.r.Writer().WriteError(w, r, err)
		return
	}

	if tokenType != fosite.AccessToken {
		errorDescription := "Only access tokens are allowed in the authorization header."
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="invalid_token",error_description="%s"`, errorDescription))
		h.r.Writer().WriteErrorCode(w, r, http.StatusUnauthorized, errors.New(errorDescription))
		return
	}

	c, ok := ar.GetClient().(*client.Client)
	if !ok {
		h.r.Writer().WriteError(w, r, errorsx.WithStack(fosite.ErrServerError.WithHint("Unable to type assert to *client.Client.")))
		return
	}

	interim := ar.GetSession().(*Session).IDTokenClaims().ToMap()
	delete(interim, "nonce")
	delete(interim, "at_hash")
	delete(interim, "c_hash")
	delete(interim, "exp")
	delete(interim, "sid")
	delete(interim, "jti")

	aud, ok := interim["aud"].([]string)
	if !ok || len(aud) == 0 {
		aud = []string{c.GetID()}
	} else {
		found := false
		for _, a := range aud {
			if a == c.GetID() {
				found = true
				break
			}
		}
		if !found {
			aud = append(aud, c.GetID())
		}
	}
	interim["aud"] = aud

	if c.UserinfoSignedResponseAlg == "RS256" {
		interim["jti"] = uuid.New()
		interim["iat"] = time.Now().Unix()

		keyID, err := h.r.OpenIDJWTStrategy().GetPublicKeyID(r.Context())
		if err != nil {
			h.r.Writer().WriteError(w, r, err)
			return
		}

		token, _, err := h.r.OpenIDJWTStrategy().Generate(ctx, jwt2.MapClaims(interim), &jwt.Headers{
			Extra: map[string]interface{}{"kid": keyID},
		})
		if err != nil {
			h.r.Writer().WriteError(w, r, err)
			return
		}

		w.Header().Set("Content-Type", "application/jwt")
		_, _ = w.Write([]byte(token))
	} else if c.UserinfoSignedResponseAlg == "" || c.UserinfoSignedResponseAlg == "none" {
		h.r.Writer().Write(w, r, interim)
	} else {
		h.r.Writer().WriteError(w, r, errorsx.WithStack(fosite.ErrServerError.WithHintf("Unsupported userinfo signing algorithm '%s'.", c.UserinfoSignedResponseAlg)))
		return
	}
}

// swagger:parameters revokeOAuth2Token
type revokeOAuth2Token struct {
	// in: formData
	// required: true
	Token string `json:"token"`
}

// swagger:route POST /oauth2/revoke v0alpha2 revokeOAuth2Token
//
// Revoke an OAuth2 Access or Refresh Token
//
// Revoking a token (both access and refresh) means that the tokens will be invalid. A revoked access token can no
// longer be used to make access requests, and a revoked refresh token can no longer be used to refresh an access token.
// Revoking a refresh token also invalidates the access token that was created with it. A token may only be revoked by
// the client the token was generated for.
//
//     Consumes:
//     - application/x-www-form-urlencoded
//
//     Schemes: http, https
//
//     Security:
//       basic:
//       oauth2:
//
//     Responses:
//       200: emptyResponse
//       default: oAuth2ApiError
func (h *Handler) revokeOAuth2Token(w http.ResponseWriter, r *http.Request) {
	var ctx = r.Context()

	err := h.r.OAuth2Provider().NewRevocationRequest(ctx, r)
	if err != nil {
		x.LogError(r, err, h.r.Logger())
	}

	h.r.OAuth2Provider().WriteRevocationResponse(ctx, w, err)
}

// swagger:parameters adminIntrospectOAuth2Token
type adminIntrospectOAuth2Token struct {
	// The string value of the token. For access tokens, this
	// is the "access_token" value returned from the token endpoint
	// defined in OAuth 2.0. For refresh tokens, this is the "refresh_token"
	// value returned.
	//
	// required: true
	// in: formData
	Token string `json:"token"`

	// An optional, space separated list of required scopes. If the access token was not granted one of the
	// scopes, the result of active will be false.
	//
	// in: formData
	Scope string `json:"scope"`
}

// swagger:route POST /admin/oauth2/introspect v0alpha2 adminIntrospectOAuth2Token
//
// Introspect OAuth2 Access or Refresh Tokens
//
// The introspection endpoint allows to check if a token (both refresh and access) is active or not. An active token
// is neither expired nor revoked. If a token is active, additional information on the token will be included. You can
// set additional data for a token by setting `accessTokenExtra` during the consent flow.
//
// For more information [read this blog post](https://www.oauth.com/oauth2-servers/token-introspection-endpoint/).
//
//     Consumes:
//     - application/x-www-form-urlencoded
//
//     Produces:
//     - application/json
//
//     Schemes: http, https
//
//     Responses:
//       200: introspectedOAuth2Token
//       default: oAuth2ApiError
func (h *Handler) adminIntrospectOAuth2Token(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var session = NewSessionWithCustomClaims("", h.c.AllowedTopLevelClaims(r.Context()))
	var ctx = r.Context()

	if r.Method != "POST" {
		err := errorsx.WithStack(fosite.ErrInvalidRequest.WithHintf("HTTP method is \"%s\", expected \"POST\".", r.Method))
		x.LogError(r, err, h.r.Logger())
		h.r.OAuth2Provider().WriteIntrospectionError(ctx, w, err)
		return
	} else if err := r.ParseMultipartForm(1 << 20); err != nil && err != http.ErrNotMultipart {
		err := errorsx.WithStack(fosite.ErrInvalidRequest.WithHint("Unable to parse HTTP body, make sure to send a properly formatted form request body.").WithDebug(err.Error()))
		x.LogError(r, err, h.r.Logger())
		h.r.OAuth2Provider().WriteIntrospectionError(ctx, w, err)
		return
	} else if len(r.PostForm) == 0 {
		err := errorsx.WithStack(fosite.ErrInvalidRequest.WithHint("The POST body can not be empty."))
		x.LogError(r, err, h.r.Logger())
		h.r.OAuth2Provider().WriteIntrospectionError(ctx, w, err)
		return
	}

	token := r.PostForm.Get("token")
	tokenType := r.PostForm.Get("token_type_hint")
	scope := r.PostForm.Get("scope")

	tt, ar, err := h.r.OAuth2Provider().IntrospectToken(ctx, token, fosite.TokenType(tokenType), session, strings.Split(scope, " ")...)
	if err != nil {
		x.LogAudit(r, err, h.r.Logger())
		err := errorsx.WithStack(fosite.ErrInactiveToken.WithHint("An introspection strategy indicated that the token is inactive.").WithDebug(err.Error()))
		h.r.OAuth2Provider().WriteIntrospectionError(ctx, w, err)
		return
	}

	resp := &fosite.IntrospectionResponse{
		Active:          true,
		AccessRequester: ar,
		TokenUse:        tt,
		AccessTokenType: "Bearer",
	}

	exp := resp.GetAccessRequester().GetSession().GetExpiresAt(tt)
	if exp.IsZero() {
		if tt == fosite.RefreshToken {
			exp = resp.GetAccessRequester().GetRequestedAt().Add(h.c.GetRefreshTokenLifespan(ctx))
		} else {
			exp = resp.GetAccessRequester().GetRequestedAt().Add(h.c.GetAccessTokenLifespan(ctx))
		}
	}

	session, ok := resp.GetAccessRequester().GetSession().(*Session)
	if !ok {
		err := errorsx.WithStack(fosite.ErrServerError.WithHint("Expected session to be of type *Session, but got another type.").WithDebug(fmt.Sprintf("Got type %s", reflect.TypeOf(resp.GetAccessRequester().GetSession()))))
		x.LogError(r, err, h.r.Logger())
		h.r.OAuth2Provider().WriteIntrospectionError(ctx, w, err)
		return
	}

	var obfuscated string
	if len(session.Claims.Subject) > 0 && session.Claims.Subject != session.Subject {
		obfuscated = session.Claims.Subject
	}

	audience := resp.GetAccessRequester().GetGrantedAudience()
	if audience == nil {
		// prevent null
		audience = fosite.Arguments{}
	}

	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	if err = json.NewEncoder(w).Encode(&Introspection{
		Active:            resp.IsActive(),
		ClientID:          resp.GetAccessRequester().GetClient().GetID(),
		Scope:             strings.Join(resp.GetAccessRequester().GetGrantedScopes(), " "),
		ExpiresAt:         exp.Unix(),
		IssuedAt:          resp.GetAccessRequester().GetRequestedAt().Unix(),
		Subject:           session.GetSubject(),
		Username:          session.GetUsername(),
		Extra:             session.Extra,
		Audience:          audience,
		Issuer:            h.c.IssuerURL(ctx).String(),
		ObfuscatedSubject: obfuscated,
		TokenType:         resp.GetAccessTokenType(),
		TokenUse:          string(resp.GetTokenUse()),
		NotBefore:         resp.GetAccessRequester().GetRequestedAt().Unix(),
	}); err != nil {
		x.LogError(r, errorsx.WithStack(err), h.r.Logger())
	}
}

// swagger:parameters performOAuth2TokenFlow
type performOAuth2TokenFlow struct {
	// in: formData
	// required: true
	GrantType string `json:"grant_type"`

	// in: formData
	Code string `json:"code"`

	// in: formData
	RefreshToken string `json:"refresh_token"`

	// in: formData
	RedirectURI string `json:"redirect_uri"`

	// in: formData
	ClientID string `json:"client_id"`
}

// OAuth2 Token Response
// swagger:model oAuth2TokenResponse
type oAuth2TokenResponse struct {
	// The lifetime in seconds of the access token.  For
	//  example, the value "3600" denotes that the access token will
	// expire in one hour from the time the response was generated.
	ExpiresIn int `json:"expires_in"`

	// The scope of the access token
	Scope int `json:"scope"`

	// To retrieve a refresh token request the id_token scope.
	IDToken int `json:"id_token"`

	// The access token issued by the authorization server.
	AccessToken string `json:"access_token"`

	// The refresh token, which can be used to obtain new
	// access tokens. To retrieve it add the scope "offline" to your access token request.
	RefreshToken string `json:"refresh_token"`

	// The type of the token issued
	TokenType string `json:"token_type"`
}

// swagger:route POST /oauth2/token v0alpha2 performOAuth2TokenFlow
//
// The OAuth 2.0 Token Endpoint
//
// The client makes a request to the token endpoint by sending the
// following parameters using the "application/x-www-form-urlencoded" HTTP
// request entity-body.
//
// > Do not implement a client for this endpoint yourself. Use a library. There are many libraries
// > available for any programming language. You can find a list of libraries here: https://oauth.net/code/
// >
// > Do note that Hydra SDK does not implement this endpoint properly. Use one of the libraries listed above
//
//     Consumes:
//     - application/x-www-form-urlencoded
//
//     Produces:
//     - application/json
//
//     Schemes: http, https
//
//     Security:
//       basic:
//       oauth2:
//
//     Responses:
//       200: oAuth2TokenResponse
//       default: oAuth2ApiError
func (h *Handler) performOAuth2TokenFlow(w http.ResponseWriter, r *http.Request) {
	var session = NewSessionWithCustomClaims("", h.c.AllowedTopLevelClaims(r.Context()))
	var ctx = r.Context()

	accessRequest, err := h.r.OAuth2Provider().NewAccessRequest(ctx, r, session)
	if err != nil {
		h.logOrAudit(err, r)
		h.r.OAuth2Provider().WriteAccessError(ctx, w, accessRequest, err)
		return
	}

	if accessRequest.GetGrantTypes().ExactOne("client_credentials") || accessRequest.GetGrantTypes().ExactOne("urn:ietf:params:oauth:grant-type:jwt-bearer") {
		var accessTokenKeyID string
		if h.c.AccessTokenStrategy(ctx) == "jwt" {
			accessTokenKeyID, err = h.r.AccessTokenJWTStrategy().GetPublicKeyID(ctx)
			if err != nil {
				x.LogError(r, err, h.r.Logger())
				h.r.OAuth2Provider().WriteAccessError(ctx, w, accessRequest, err)
				return
			}
		}

		// only for client_credentials, otherwise Authentication is included in session
		if accessRequest.GetGrantTypes().ExactOne("client_credentials") {
			session.Subject = accessRequest.GetClient().GetID()
		}
		session.ClientID = accessRequest.GetClient().GetID()
		session.KID = accessTokenKeyID
		session.DefaultSession.Claims.Issuer = h.c.IssuerURL(r.Context()).String()
		session.DefaultSession.Claims.IssuedAt = time.Now().UTC()

		var scopes = accessRequest.GetRequestedScopes()

		// Added for compatibility with MITREid
		if h.c.GrantAllClientCredentialsScopesPerDefault(r.Context()) && len(scopes) == 0 {
			for _, scope := range accessRequest.GetClient().GetScopes() {
				accessRequest.GrantScope(scope)
			}
		}

		for _, scope := range scopes {
			if h.r.Config().GetScopeStrategy(ctx)(accessRequest.GetClient().GetScopes(), scope) {
				accessRequest.GrantScope(scope)
			}
		}

		for _, audience := range accessRequest.GetRequestedAudience() {
			if h.r.AudienceStrategy()(accessRequest.GetClient().GetAudience(), []string{audience}) == nil {
				accessRequest.GrantAudience(audience)
			}
		}
	}

	for _, hook := range h.r.AccessRequestHooks() {
		if err := hook(ctx, accessRequest); err != nil {
			h.logOrAudit(err, r)
			h.r.OAuth2Provider().WriteAccessError(ctx, w, accessRequest, err)
			return
		}
	}

	accessResponse, err := h.r.OAuth2Provider().NewAccessResponse(ctx, accessRequest)
	if err != nil {
		h.logOrAudit(err, r)
		h.r.OAuth2Provider().WriteAccessError(ctx, w, accessRequest, err)
		return
	}

	h.r.OAuth2Provider().WriteAccessResponse(ctx, w, accessRequest, accessResponse)
}

// swagger:route GET /oauth2/auth v0alpha2 performOAuth2AuthorizationFlow
//
// The OAuth 2.0 Authorize Endpoint
//
// This endpoint is not documented here because you should never use your own implementation to perform OAuth2 flows.
// OAuth2 is a very popular protocol and a library for your programming language will exists.
//
// To learn more about this flow please refer to the specification: https://tools.ietf.org/html/rfc6749
//
//     Consumes:
//     - application/x-www-form-urlencoded
//
//     Schemes: http, https
//
//     Responses:
//       302: emptyResponse
//       default: oAuth2ApiError
func (h *Handler) performOAuth2AuthorizationFlow(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var ctx = r.Context()

	authorizeRequest, err := h.r.OAuth2Provider().NewAuthorizeRequest(ctx, r)
	if err != nil {
		x.LogError(r, err, h.r.Logger())
		h.writeAuthorizeError(w, r, authorizeRequest, err)
		return
	}

	session, err := h.r.ConsentStrategy().HandleOAuth2AuthorizationRequest(ctx, w, r, authorizeRequest)
	if errors.Is(err, consent.ErrAbortOAuth2Request) {
		x.LogAudit(r, nil, h.r.AuditLogger())
		// do nothing
		return
	} else if e := &(fosite.RFC6749Error{}); errors.As(err, &e) {
		x.LogAudit(r, err, h.r.AuditLogger())
		h.writeAuthorizeError(w, r, authorizeRequest, err)
		return
	} else if err != nil {
		x.LogError(r, err, h.r.Logger())
		h.writeAuthorizeError(w, r, authorizeRequest, err)
		return
	}

	for _, scope := range session.GrantedScope {
		authorizeRequest.GrantScope(scope)
	}

	for _, audience := range session.GrantedAudience {
		authorizeRequest.GrantAudience(audience)
	}

	openIDKeyID, err := h.r.OpenIDJWTStrategy().GetPublicKeyID(ctx)
	if err != nil {
		x.LogError(r, err, h.r.Logger())
		h.writeAuthorizeError(w, r, authorizeRequest, err)
		return
	}

	var accessTokenKeyID string
	if h.c.AccessTokenStrategy(r.Context()) == "jwt" {
		accessTokenKeyID, err = h.r.AccessTokenJWTStrategy().GetPublicKeyID(ctx)
		if err != nil {
			x.LogError(r, err, h.r.Logger())
			h.writeAuthorizeError(w, r, authorizeRequest, err)
			return
		}
	}

	obfuscatedSubject, err := h.r.ConsentStrategy().ObfuscateSubjectIdentifier(ctx, authorizeRequest.GetClient(), session.ConsentRequest.Subject, session.ConsentRequest.ForceSubjectIdentifier)
	if e := &(fosite.RFC6749Error{}); errors.As(err, &e) {
		x.LogAudit(r, err, h.r.AuditLogger())
		h.writeAuthorizeError(w, r, authorizeRequest, err)
		return
	} else if err != nil {
		x.LogError(r, err, h.r.Logger())
		h.writeAuthorizeError(w, r, authorizeRequest, err)
		return
	}

	authorizeRequest.SetID(session.ID)
	claims := &jwt.IDTokenClaims{
		Subject:                             obfuscatedSubject,
		Issuer:                              h.c.IssuerURL(ctx).String(),
		AuthTime:                            time.Time(session.AuthenticatedAt),
		RequestedAt:                         session.RequestedAt,
		Extra:                               session.Session.IDToken,
		AuthenticationContextClassReference: session.ConsentRequest.ACR,
		AuthenticationMethodsReferences:     session.ConsentRequest.AMR,

		// These are required for work around https://github.com/ory/fosite/issues/530
		Nonce:    authorizeRequest.GetRequestForm().Get("nonce"),
		Audience: []string{authorizeRequest.GetClient().GetID()},
		IssuedAt: time.Now().Truncate(time.Second).UTC(),

		// This is set by the fosite strategy
		// ExpiresAt:   time.Now().Add(h.IDTokenLifespan).UTC(),
	}
	claims.Add("sid", session.ConsentRequest.LoginSessionID)

	// done
	response, err := h.r.OAuth2Provider().NewAuthorizeResponse(ctx, authorizeRequest, &Session{
		DefaultSession: &openid.DefaultSession{
			Claims: claims,
			Headers: &jwt.Headers{Extra: map[string]interface{}{
				// required for lookup on jwk endpoint
				"kid": openIDKeyID,
			}},
			Subject: session.ConsentRequest.Subject,
		},
		Extra:                 session.Session.AccessToken,
		KID:                   accessTokenKeyID,
		ClientID:              authorizeRequest.GetClient().GetID(),
		ConsentChallenge:      session.ID,
		ExcludeNotBeforeClaim: h.c.ExcludeNotBeforeClaim(ctx),
		AllowedTopLevelClaims: h.c.AllowedTopLevelClaims(ctx),
	})
	if err != nil {
		x.LogError(r, err, h.r.Logger())
		h.writeAuthorizeError(w, r, authorizeRequest, err)
		return
	}

	h.r.OAuth2Provider().WriteAuthorizeResponse(ctx, w, authorizeRequest, response)
}

// swagger:parameters adminDeleteOAuth2Token
type adminDeleteOAuth2Token struct {
	//required: true
	// in: query
	ClientID string `json:"client_id"`
}

// swagger:route DELETE /admin/oauth2/tokens v0alpha2 adminDeleteOAuth2Token
//
// Delete OAuth2 Access Tokens from a Client
//
// This endpoint deletes OAuth2 access tokens issued for a client from the database
//
//     Consumes:
//     - application/json
//
//     Schemes: http, https
//
//     Responses:
//       204: emptyResponse
//       default: oAuth2ApiError
func (h *Handler) adminDeleteOAuth2Token(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		h.r.Writer().WriteError(w, r, errorsx.WithStack(fosite.ErrInvalidRequest.WithHint(`Query parameter 'client_id' is not defined but it should have been.`)))
		return
	}

	if err := h.r.OAuth2Storage().DeleteAccessTokens(r.Context(), clientID); err != nil {
		h.r.Writer().WriteError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// This function will not be called, OPTIONS request will be handled by cors
// this is just a placeholder.
func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) {}

func (h *Handler) forwardError(w http.ResponseWriter, r *http.Request, err error) {
	rfcErr := fosite.ErrorToRFC6749Error(err).WithExposeDebug(h.c.GetSendDebugMessagesToClients(r.Context()))
	query := rfcErr.ToValues()
	http.Redirect(w, r, urlx.CopyWithQuery(h.c.ErrorURL(r.Context()), query).String(), http.StatusFound)
}

func (h *Handler) writeAuthorizeError(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, err error) {
	if !ar.IsRedirectURIValid() {
		h.forwardError(w, r, err)
		return
	}

	h.r.OAuth2Provider().WriteAuthorizeError(r.Context(), w, ar, err)
}

func (h *Handler) logOrAudit(err error, r *http.Request) {
	if errors.Is(err, fosite.ErrServerError) || errors.Is(err, fosite.ErrTemporarilyUnavailable) || errors.Is(err, fosite.ErrMisconfiguration) {
		x.LogError(r, err, h.r.Logger())
	} else {
		x.LogAudit(r, err, h.r.Logger())
	}
}

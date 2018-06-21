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

package server

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/sessions"
	"github.com/julienschmidt/httprouter"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/ory/fosite/handler/openid"
	"github.com/ory/herodot"
	"github.com/ory/hydra/client"
	"github.com/ory/hydra/config"
	"github.com/ory/hydra/consent"
	"github.com/ory/hydra/jwk"
	"github.com/ory/hydra/oauth2"
	"github.com/ory/hydra/pkg"
	"github.com/ory/sqlcon"
	"github.com/pborman/uuid"
)

func injectFositeStore(c *config.Config, clients client.Manager) {
	var ctx = c.Context()
	var store pkg.FositeStorer

	switch con := ctx.Connection.(type) {
	case *config.MemoryConnection:
		store = oauth2.NewFositeMemoryStore(clients, c.GetAccessTokenLifespan())
		break
	case *sqlcon.SQLConnection:
		store = oauth2.NewFositeSQLStore(clients, con.GetDatabase(), c.GetLogger(), c.GetAccessTokenLifespan())
		break
	case *config.PluginConnection:
		var err error
		if store, err = con.NewOAuth2Manager(clients); err != nil {
			c.GetLogger().Fatalf("Could not load client manager plugin %s", err)
		}
		break
	default:
		panic("Unknown connection type.")
	}

	ctx.FositeStore = store
}

func newOAuth2Provider(c *config.Config) (fosite.OAuth2Provider, string) {
	var ctx = c.Context()
	var store = ctx.FositeStore

	kid := uuid.New()
	privateKey, err := createOrGetJWK(c, oauth2.OpenIDConnectKeyName, kid, "private")
	if err != nil {
		c.GetLogger().WithError(err).Fatalf(`Could not fetch private signing key for OpenID Connect - did you forget to run "hydra migrate sql" or forget to set the SYSTEM_SECRET?`)
	}

	publicKey, err := createOrGetJWK(c, oauth2.OpenIDConnectKeyName, kid, "public")
	if err != nil {
		c.GetLogger().WithError(err).Fatalf(`Could not fetch public signing key for OpenID Connect - did you forget to run "hydra migrate sql" or forget to set the SYSTEM_SECRET?`)
	}

	fc := &compose.Config{
		AccessTokenLifespan:            c.GetAccessTokenLifespan(),
		AuthorizeCodeLifespan:          c.GetAuthCodeLifespan(),
		IDTokenLifespan:                c.GetIDTokenLifespan(),
		HashCost:                       c.BCryptWorkFactor,
		ScopeStrategy:                  c.GetScopeStrategy(),
		SendDebugMessagesToClients:     c.SendOAuth2DebugMessagesToClients,
		EnforcePKCE:                    false,
		EnablePKCEPlainChallengeMethod: false,
		TokenURL:                       strings.TrimRight(c.Issuer, "/") + oauth2.TokenPath,
	}

	jwtStrategy := compose.NewOpenIDConnectStrategy(jwk.MustRSAPrivate(privateKey))
	return compose.Compose(
		fc,
		store,
		&compose.CommonStrategy{
			CoreStrategy:               compose.NewOAuth2HMACStrategy(fc, c.GetSystemSecret()),
			OpenIDConnectTokenStrategy: jwtStrategy,
			JWTStrategy:                jwtStrategy,
		},
		nil,
		compose.OAuth2AuthorizeExplicitFactory,
		compose.OAuth2AuthorizeImplicitFactory,
		compose.OAuth2ClientCredentialsGrantFactory,
		compose.OAuth2RefreshTokenGrantFactory,
		compose.OAuth2PKCEFactory,
		compose.OpenIDConnectExplicitFactory,
		compose.OpenIDConnectHybridFactory,
		compose.OpenIDConnectImplicitFactory,
		compose.OpenIDConnectRefreshFactory,
		compose.OAuth2TokenRevocationFactory,
		compose.OAuth2TokenIntrospectionFactory,
	), publicKey.KeyID
}

func setDefaultConsentURL(s string, c *config.Config, path string) string {
	if s != "" {
		return s
	}
	proto := "https"
	if c.ForceHTTP {
		proto = "http"
	}
	host := "localhost"
	if c.BindHost != "" {
		host = c.BindHost
	}
	return fmt.Sprintf("%s://%s:%d/%s", proto, host, c.BindPort, path)
}

//func newOAuth2Handler(c *config.Config, router *httprouter.Router, cm oauth2.ConsentRequestManager, o fosite.OAuth2Provider, idTokenKeyID string) *oauth2.Handler {
func newOAuth2Handler(c *config.Config, router *httprouter.Router, cm consent.Manager, o fosite.OAuth2Provider, idTokenKeyID string) *oauth2.Handler {
	c.ConsentURL = setDefaultConsentURL(c.ConsentURL, c, "oauth2/fallbacks/consent")
	c.LoginURL = setDefaultConsentURL(c.LoginURL, c, "oauth2/fallbacks/consent")
	c.ErrorURL = setDefaultConsentURL(c.ErrorURL, c, "oauth2/fallbacks/error")

	errorURL, err := url.Parse(c.ErrorURL)
	pkg.Must(err, "Could not parse error url %s.", errorURL)

	privateKey, err := createOrGetJWK(c, oauth2.OpenIDConnectKeyName, "", "private")
	if err != nil {
		c.GetLogger().WithError(err).Fatalf(`Could not fetch private signing key for OpenID Connect - did you forget to run "hydra migrate sql" or forget to set the SYSTEM_SECRET?`)
	}

	jwtStrategy := compose.NewOpenIDConnectStrategy(jwk.MustRSAPrivate(privateKey))

	w := herodot.NewJSONWriter(c.GetLogger())
	w.ErrorEnhancer = writerErrorEnhancer

	handler := &oauth2.Handler{
		ScopesSupported:  c.OpenIDDiscoveryScopesSupported,
		UserinfoEndpoint: c.OpenIDDiscoveryUserinfoEndpoint,
		ClaimsSupported:  c.OpenIDDiscoveryClaimsSupported,
		ForcedHTTP:       c.ForceHTTP,
		OAuth2:           o,
		ScopeStrategy:    c.GetScopeStrategy(),
		Consent: consent.NewStrategy(
			c.LoginURL, c.ConsentURL, c.Issuer,
			"/oauth2/auth", cm,
			sessions.NewCookieStore(c.GetCookieSecret()), c.GetScopeStrategy(),
			!c.ForceHTTP, time.Minute*15,
			jwtStrategy,
			openid.NewOpenIDConnectRequestValidator(nil, jwtStrategy),
		),
		Storage:             c.Context().FositeStore,
		ErrorURL:            *errorURL,
		H:                   w,
		AccessTokenLifespan: c.GetAccessTokenLifespan(),
		CookieStore:         sessions.NewCookieStore(c.GetCookieSecret()),
		IssuerURL:           c.Issuer,
		L:                   c.GetLogger(),
		IDTokenPublicKeyID:  idTokenKeyID,
		IDTokenLifespan:     c.GetIDTokenLifespan(),
	}

	go func() {
		for {
			time.Sleep(time.Second * 5)

			publicKey, err := getJWK(c, oauth2.OpenIDConnectKeyName, "public")
			if err != nil {
				c.GetLogger().WithError(err).Error("Unable to refresh OpenID Connect signing keys, retrying...")
				continue
			}

			privateKey, err := getJWK(c, oauth2.OpenIDConnectKeyName, "private")
			if err != nil {
				c.GetLogger().WithError(err).Error("Unable to refresh OpenID Connect signing keys, retrying...")
				continue
			}

			handler.IDTokenPublicKeyID = publicKey.KeyID
			jwtStrategy.PrivateKey = jwk.MustRSAPrivate(privateKey)
		}
	}()

	handler.SetRoutes(router)
	return handler
}

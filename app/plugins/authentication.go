package plugins

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/crypto/bcrypt"
)

const (
	authPluginName = "authentication"
)

// UserContextKey is the key for storing user info in the request context.
const UserContextKey contextKeyType = "userInfo"

type contextKeyType string

// AuthenticatedUser holds information about the authenticated user.
type AuthenticatedUser struct {
	ID     string
	Claims jwt.MapClaims // Or a custom claims struct if you have one
}

// HashedAPIKeyDetails holds the securely stored components of an API key.
type HashedAPIKeyDetails struct {
	ClientID   string // Optional identifier for the client associated with this key.
	BcryptHash []byte // The bcrypt hash of the original plaintext API key.
}

// AuthenticationPlugin handles request authentication.
type AuthenticationPlugin struct {
	Provider string // e.g., "jwt", "api-key", "none"

	// JWT Specific fields
	jwtSecretKey         []byte
	jwtPublicKey         crypto.PublicKey
	jwtExpectedAlgorithm string
	jwtExpectedIssuer    string
	jwtExpectedAudience  string
	tokenHeaderName      string
	tokenHeaderScheme    string

	// API Key Specific fields
	validAPIKeys       map[string]HashedAPIKeyDetails // Key: lookup_key (SHA256 of original key), Value: HashedAPIKeyDetails
	apiKeyHeaderName   string
	mongoURIFromConfig string // Store the URI parsed from config

	// MongoDB client for API Key provider
	mongoClient                  *mongo.Client
	mongoDatabaseName            string        // Renamed for clarity
	mongoCollectionName          string        // Renamed for clarity
	mongoOperationContextTimeout time.Duration // Renamed for clarity
	useMongoDBForAPIKeys         bool
	apiKeyRefreshInterval        time.Duration
	apiKeyRefreshTicker          *time.Ticker
	stopAPIKeyRefresh            chan struct{}
	apiKeysMutex                 sync.RWMutex // To protect validAPIKeys map

}

// init registers the AuthenticationPlugin.
func init() {
	RegisterPlugin(authPluginName, func() Plugin {
		return &AuthenticationPlugin{}
	})
}

// Name returns the plugin's name.
func (p *AuthenticationPlugin) Name() string {
	return authPluginName
}

// Init initializes the AuthenticationPlugin with its configuration.
// It checks for a 'provider' in the route-specific config, then global config.
func (p *AuthenticationPlugin) Init(pluginConfig map[string]any, globalPluginsConfig *config.PluginsConfig) error {

	logging.Debugf("Plugin [%s]: Initializing...", p.Name())

	// Set defaults
	p.Provider = "none"
	p.tokenHeaderName = "Authorization"
	p.tokenHeaderScheme = "Bearer"
	p.apiKeyHeaderName = "X-API-Key"                 // Default header for API keys
	p.mongoOperationContextTimeout = 5 * time.Second // Default MongoDB operation timeout
	p.apiKeyRefreshInterval = 5 * time.Minute        // Default API key refresh interval
	p.stopAPIKeyRefresh = make(chan struct{})

	// Apply global plugin configurations first
	if globalPluginsConfig != nil && globalPluginsConfig.Authentication != nil {
		p.applyAuthConfig(globalPluginsConfig.Authentication, "global")
	}

	// Override with route-specific plugin configurations
	if pluginConfig != nil {
		p.applyAuthConfig(pluginConfig, "route-specific")
	}

	// Validate JWT configuration if provider is jwt
	if p.Provider == "jwt" {
		if p.jwtExpectedAlgorithm == "" {
			return fmt.Errorf("plugin [%s]: JWT provider selected but 'jwt_algorithm' is not configured", p.Name())
		}
		isHmac := strings.HasPrefix(p.jwtExpectedAlgorithm, "HS")
		isRsaEcdsaEdDsa := strings.HasPrefix(p.jwtExpectedAlgorithm, "RS") || strings.HasPrefix(p.jwtExpectedAlgorithm, "ES") || strings.HasPrefix(p.jwtExpectedAlgorithm, "PS")

		if isHmac && p.jwtSecretKey == nil {
			return fmt.Errorf("plugin [%s]: JWT algorithm '%s' requires 'jwt_secret' to be configured", p.Name(), p.jwtExpectedAlgorithm)
		}
		if isRsaEcdsaEdDsa && p.jwtPublicKey == nil {
			return fmt.Errorf("plugin [%s]: JWT algorithm '%s' requires 'jwt_public_key_file' to be configured and valid", p.Name(), p.jwtExpectedAlgorithm)
		}
		if !isHmac && !isRsaEcdsaEdDsa {
			return fmt.Errorf("plugin [%s]: Unsupported JWT algorithm configured: %s", p.Name(), p.jwtExpectedAlgorithm)
		}
	}

	// If API key provider is selected and MongoDB is configured, try to connect
	if p.Provider == "api-key" && p.useMongoDBForAPIKeys {
		err := p.connectMongoDB()
		if err != nil {
			// Log the error but don't necessarily fail startup,
			// unless strict mode for plugin init is desired.
			// The plugin might fall back to YAML-configured keys or no keys.
			logging.Errorf("Plugin [%s]: Failed to connect to MongoDB for API keys: %v. API key auth may be degraded.", p.Name(), err)
			p.useMongoDBForAPIKeys = false // Fallback: don't use MongoDB
		} else {
			logging.Infof("Plugin [%s]: Successfully connected to MongoDB for API key provider.", p.Name())
			// Load keys from MongoDB
			err = p.loadAPIKeysFromMongoDB()
			if err != nil {
				logging.Errorf("Plugin [%s]: Initial load of API keys from MongoDB failed: %v. API key auth may be degraded.", p.Name(), err)
			} else if p.apiKeyRefreshInterval > 0 {
				// Start periodic refresh only if initial load was successful and interval is positive
				p.apiKeyRefreshTicker = time.NewTicker(p.apiKeyRefreshInterval)
				go p.periodicAPIKeyRefresh()
			}
		}
	}

	logging.Infof("Plugin [%s]: Configured with provider '%s'", p.Name(), p.Provider)
	return nil
}

// applyAuthConfig helper to parse configuration map.
func (p *AuthenticationPlugin) applyAuthConfig(configMap map[string]any, scope string) {
	if provider, ok := configMap["provider"].(string); ok && provider != "" {
		p.Provider = strings.ToLower(provider)
		logging.Debugf("Plugin [%s]: Set provider to '%s' from %s config", p.Name(), p.Provider, scope)
	}
	if secret, ok := configMap["jwt_secret"].(string); ok && secret != "" {
		p.jwtSecretKey = []byte(secret)
		logging.Debugf("Plugin [%s]: Loaded jwt_secret from %s config", p.Name(), scope)
	}
	if algo, ok := configMap["jwt_algorithm"].(string); ok && algo != "" {
		p.jwtExpectedAlgorithm = algo
		logging.Debugf("Plugin [%s]: Set jwt_algorithm to '%s' from %s config", p.Name(), p.jwtExpectedAlgorithm, scope)
	}
	if issuer, ok := configMap["jwt_issuer"].(string); ok && issuer != "" {
		p.jwtExpectedIssuer = issuer
		logging.Debugf("Plugin [%s]: Set jwt_issuer to '%s' from %s config", p.Name(), p.jwtExpectedIssuer, scope)
	}
	if audience, ok := configMap["jwt_audience"].(string); ok && audience != "" {
		p.jwtExpectedAudience = audience
		logging.Debugf("Plugin [%s]: Set jwt_audience to '%s' from %s config", p.Name(), p.jwtExpectedAudience, scope)
	}
	if headerName, ok := configMap["token_header_name"].(string); ok && headerName != "" {
		p.tokenHeaderName = headerName
		logging.Debugf("Plugin [%s]: Set token_header_name to '%s' from %s config", p.Name(), p.tokenHeaderName, scope)
	}
	if headerScheme, ok := configMap["token_header_scheme"].(string); ok && headerScheme != "" {
		p.tokenHeaderScheme = headerScheme
		logging.Debugf("Plugin [%s]: Set token_header_scheme to '%s' from %s config", p.Name(), p.tokenHeaderScheme, scope)
	}
	if apiKeyHeader, ok := configMap["api_key_header_name"].(string); ok && apiKeyHeader != "" {
		p.apiKeyHeaderName = apiKeyHeader
		logging.Debugf("Plugin [%s]: Set api_key_header_name to '%s' from %s config", p.Name(), p.apiKeyHeaderName, scope)
	}

	// MongoDB Configuration Parsing
	if mongoURI, ok := configMap["mongodb_uri"].(string); ok && mongoURI != "" {
		p.mongoURIFromConfig = mongoURI // Store for connectMongoDB
		p.useMongoDBForAPIKeys = true   // Mark that MongoDB should be attempted

		if dbName, ok := configMap["mongodb_database"].(string); ok && dbName != "" {
			p.mongoDatabaseName = dbName
		} else {
			logging.Warnf("Plugin [%s]: mongodb_uri is set but mongodb_database is missing in %s config. MongoDB for API keys might not work.", p.Name(), scope)
			p.useMongoDBForAPIKeys = false
		}
		if collName, ok := configMap["mongodb_collection_apikeys"].(string); ok && collName != "" {
			p.mongoCollectionName = collName
		} else {
			logging.Warnf("Plugin [%s]: mongodb_uri is set but mongodb_collection_apikeys is missing in %s config. MongoDB for API keys might not work.", p.Name(), scope)
			p.useMongoDBForAPIKeys = false
		}
		if timeoutMs, ok := configMap["mongodb_timeout_ms"].(int); ok && timeoutMs > 0 {
			p.mongoOperationContextTimeout = time.Duration(timeoutMs) * time.Millisecond
		}
		if refreshIntervalSec, ok := configMap["mongodb_apikeys_refresh_interval_sec"].(int); ok && refreshIntervalSec > 0 {
			p.apiKeyRefreshInterval = time.Duration(refreshIntervalSec) * time.Second
		}
		// Only log if we intend to use MongoDB based on successful parsing of essential fields
		if p.useMongoDBForAPIKeys {
			logging.Debugf("Plugin [%s]: MongoDB settings parsed from %s config (DB: %s, Collection: %s, Timeout: %v)", p.Name(), scope, p.mongoDatabaseName, p.mongoCollectionName, p.mongoOperationContextTimeout)
		}
	} else if scope == "global" { // If mongo_uri is not in global, it can't be used.
		p.useMongoDBForAPIKeys = false
	}

	// Load API keys from YAML only if MongoDB is not configured or connection fails
	if !p.useMongoDBForAPIKeys && scope == "global" { // YAML keys are typically global, not per-route if DB is primary
		p.apiKeysMutex.Lock()
		if keysList, ok := configMap["api_keys"].([]interface{}); ok {
			if p.validAPIKeys == nil {
				p.validAPIKeys = make(map[string]HashedAPIKeyDetails)
			}
			for _, keyEntry := range keysList {
				if keyMap, ok := keyEntry.(map[string]interface{}); ok {
					lookupKey, lok := keyMap["lookup_key"].(string)
					hashedValue, hvok := keyMap["hashed_value"].(string)
					clientID, _ := keyMap["client_id"].(string) // Optional

					if lok && hvok && lookupKey != "" && hashedValue != "" {
						p.validAPIKeys[lookupKey] = HashedAPIKeyDetails{ClientID: clientID, BcryptHash: []byte(hashedValue)}
						logging.Debugf("Plugin [%s]: Loaded hashed API key from YAML for ClientID: '%s' (LookupKey: %s...) from %s config", p.Name(), clientID, lookupKey[:8], scope)
					} else {
						logging.Warnf("Plugin [%s]: Skipping API key entry from YAML due to missing 'lookup_key' or 'hashed_value' in %s config: %+v", p.Name(), scope, keyMap)
					}
				}
			}
		}
		p.apiKeysMutex.Unlock()
	}

	if pubKeyFile, ok := configMap["jwt_public_key_file"].(string); ok && pubKeyFile != "" {
		keyBytes, err := os.ReadFile(pubKeyFile) // Changed ioutil.ReadFile to os.ReadFile
		if err != nil {
			logging.Errorf("Plugin [%s]: Failed to read jwt_public_key_file '%s' from %s config: %v", p.Name(), pubKeyFile, scope, err)
			return
		}
		block, _ := pem.Decode(keyBytes)
		if block == nil {
			logging.Errorf("Plugin [%s]: Failed to decode PEM block from jwt_public_key_file '%s' in %s config", p.Name(), pubKeyFile, scope)
			return
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			// Try parsing as PKCS1 an RSA public key if PKIX fails
			pub, err = x509.ParsePKCS1PublicKey(block.Bytes)
			if err != nil {
				logging.Errorf("Plugin [%s]: Failed to parse public key from jwt_public_key_file '%s' (tried PKIX & PKCS1) in %s config: %v", p.Name(), pubKeyFile, scope, err)
				return
			}
		}
		switch pub := pub.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
			p.jwtPublicKey = pub
			logging.Debugf("Plugin [%s]: Successfully loaded JWT public key from '%s' (%s config)", p.Name(), pubKeyFile, scope)
		default:
			logging.Errorf("Plugin [%s]: Unsupported public key type in jwt_public_key_file '%s' (%s config): %T", p.Name(), pubKeyFile, scope, pub)
		}
	}

	logging.Infof("Plugin [%s]: Configured with provider '%s'", p.Name(), p.Provider)
	return
}

// connectMongoDB establishes a connection to the MongoDB server.
func (p *AuthenticationPlugin) connectMongoDB() error {
	if p.mongoURIFromConfig == "" {
		return fmt.Errorf("mongodb_uri not configured")
	}

	clientOptions := options.Client().ApplyURI(p.mongoURIFromConfig)
	ctx, cancel := context.WithTimeout(context.Background(), p.mongoOperationContextTimeout) // Use connection timeout
	defer cancel()

	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		return fmt.Errorf("failed to connect to mongo: %w", err)
	}

	// Ping the primary
	err = client.Ping(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to ping mongo: %w", err)
	}

	p.mongoClient = client
	return nil
}

// loadAPIKeysFromMongoDB fetches API key details from the MongoDB collection.
func (p *AuthenticationPlugin) loadAPIKeysFromMongoDB() error {
	if p.mongoClient == nil || p.mongoDatabaseName == "" || p.mongoCollectionName == "" {
		return fmt.Errorf("mongodb client or configuration not initialized")
	}

	collection := p.mongoClient.Database(p.mongoDatabaseName).Collection(p.mongoCollectionName)
	ctx, cancel := context.WithTimeout(context.Background(), p.mongoOperationContextTimeout)
	defer cancel()

	cursor, err := collection.Find(ctx, bson.M{"status": "active"}) // Example: only load active keys
	if err != nil {
		return fmt.Errorf("failed to query api_keys collection: %w", err)
	}
	defer cursor.Close(ctx)

	p.apiKeysMutex.Lock()         // Lock before modifying the shared map
	defer p.apiKeysMutex.Unlock() // Ensure unlock even on panic

	loadedKeys := make(map[string]HashedAPIKeyDetails)
	for cursor.Next(ctx) {
		var apiKeyDoc struct {
			ClientID    string `bson:"client_id"`
			LookupKey   string `bson:"lookup_key"`   // Should be indexed in MongoDB
			HashedValue string `bson:"hashed_value"` // Stored as string in DB, convert to []byte
			// Status      string `bson:"status"` // Already filtered by query
		}
		if err := cursor.Decode(&apiKeyDoc); err != nil {
			logging.Warnf("Plugin [%s]: Failed to decode API key document from MongoDB: %v", p.Name(), err)
			continue
		}
		loadedKeys[apiKeyDoc.LookupKey] = HashedAPIKeyDetails{ClientID: apiKeyDoc.ClientID, BcryptHash: []byte(apiKeyDoc.HashedValue)}
		logging.Debugf("Plugin [%s]: Loaded hashed API key from MongoDB for ClientID: '%s' (LookupKey: %s...)", p.Name(), apiKeyDoc.ClientID, apiKeyDoc.LookupKey[:8])
	}
	p.validAPIKeys = loadedKeys // Replace in-memory cache
	logging.Infof("Plugin [%s]: Loaded %d API keys from MongoDB.", p.Name(), len(p.validAPIKeys))
	return nil
}

// periodicAPIKeyRefresh is run in a goroutine to periodically refresh keys.
func (p *AuthenticationPlugin) periodicAPIKeyRefresh() {
	logging.Infof("Plugin [%s]: Starting periodic API key refresh every %v", p.Name(), p.apiKeyRefreshInterval)
	for {
		select {
		case <-p.apiKeyRefreshTicker.C:
			logging.Debugf("Plugin [%s]: Refreshing API keys from MongoDB...", p.Name())
			err := p.loadAPIKeysFromMongoDB()
			if err != nil {
				logging.Errorf("Plugin [%s]: Error during periodic API key refresh: %v", p.Name(), err)
			}
		case <-p.stopAPIKeyRefresh:
			logging.Infof("Plugin [%s]: Stopping periodic API key refresh.", p.Name())
			p.apiKeyRefreshTicker.Stop()
			return
		}
	}
}

// Handle is the middleware function for the AuthenticationPlugin.
func (p *AuthenticationPlugin) Handle(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		logging.Debugf("Plugin [%s]: Handling request with provider '%s'", p.Name(), p.Provider)

		switch p.Provider {

		case "jwt":
			authHeader := r.Header.Get(p.tokenHeaderName)
			if authHeader == "" {
				logging.Warnf("Plugin [%s]: JWT provider - Missing '%s' header", p.Name(), p.tokenHeaderName)
				http.Error(w, "Unauthorized: Missing or malformed token", http.StatusUnauthorized)
				return
			}

			var tokenString string
			parts := strings.Fields(authHeader)
			if len(parts) == 2 && strings.EqualFold(parts[0], p.tokenHeaderScheme) {
				tokenString = parts[1]
			} else if len(parts) == 1 && p.tokenHeaderScheme == "" { // No scheme expected, token is the whole header
				tokenString = parts[0]
			} else {
				logging.Warnf("Plugin [%s]: JWT provider - Malformed '%s' header or incorrect scheme. Expected scheme (case-insensitive): '%s'", p.Name(), p.tokenHeaderName, p.tokenHeaderScheme)
				http.Error(w, "Unauthorized: Malformed token", http.StatusUnauthorized)
				return
			}

			if tokenString == "" {
				logging.Warnf("Plugin [%s]: JWT provider - Empty token string after parsing header", p.Name())
				http.Error(w, "Unauthorized: Invalid token", http.StatusUnauthorized)
				return
			}

			keyFunc := func(token *jwt.Token) (interface{}, error) {
				// Check if the token's algorithm matches the configured one.
				// The jwt.WithValidMethods parser option already does this,
				// but this is an explicit check within the keyFunc.
				if alg, ok := token.Header["alg"].(string); !ok || alg != p.jwtExpectedAlgorithm {
					return nil, fmt.Errorf("unexpected signing method: %v, expected %s", token.Header["alg"], p.jwtExpectedAlgorithm)
				}

				switch token.Method.(type) {
				case *jwt.SigningMethodHMAC:
					return p.jwtSecretKey, nil
				case *jwt.SigningMethodRSA, *jwt.SigningMethodECDSA, *jwt.SigningMethodEd25519:
					return p.jwtPublicKey, nil
				default:
					return nil, fmt.Errorf("unsupported signing method: %v", token.Header["alg"])
				}
			}

			parserOpts := []jwt.ParserOption{
				jwt.WithValidMethods([]string{p.jwtExpectedAlgorithm}),
			}
			if p.jwtExpectedIssuer != "" {
				parserOpts = append(parserOpts, jwt.WithIssuer(p.jwtExpectedIssuer))
			}
			if p.jwtExpectedAudience != "" {
				parserOpts = append(parserOpts, jwt.WithAudience(p.jwtExpectedAudience))
			}
			// parserOpts = append(parserOpts, jwt.WithExpirationRequired()) // Enforce 'exp' claim

			token, err := jwt.ParseWithClaims(tokenString, jwt.MapClaims{}, keyFunc, parserOpts...)

			if err != nil {
				logging.Warnf("Plugin [%s]: JWT validation error: %v", p.Name(), err)
				// Avoid overly specific error messages to the client
				http.Error(w, "Unauthorized: Invalid token", http.StatusUnauthorized)
				return
			}

			if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
				logging.Debugf("Plugin [%s]: JWT validated successfully. Sub: %v", p.Name(), claims["sub"])
				authenticatedUser := AuthenticatedUser{ID: fmt.Sprintf("%v", claims["sub"]), Claims: claims}
				r = r.WithContext(context.WithValue(r.Context(), UserContextKey, authenticatedUser))
			} else {
				logging.Warnf("Plugin [%s]: JWT token invalid or claims parsing failed after successful parse", p.Name())
				http.Error(w, "Unauthorized: Invalid token", http.StatusUnauthorized)
				return
			}

		case "api-key":

			incomingPlaintextKey := r.Header.Get(p.apiKeyHeaderName)
			if incomingPlaintextKey == "" {
				logging.Warnf("Plugin [%s]: API-Key provider - Missing '%s' header", p.Name(), p.apiKeyHeaderName)
				http.Error(w, "Unauthorized: Missing API Key", http.StatusUnauthorized)
				return
			}

			// 1. Compute SHA256 of the incoming plaintext key to get the lookup key
			hasher := sha256.New()
			hasher.Write([]byte(incomingPlaintextKey))
			incomingLookupKey := hex.EncodeToString(hasher.Sum(nil))

			// 2. Look up using the SHA256 hash
			p.apiKeysMutex.RLock() // Read lock for accessing validAPIKeys
			apiKeyDetails, found := p.validAPIKeys[incomingLookupKey]
			p.apiKeysMutex.RUnlock()
			if !found {
				logging.Warnf("Plugin [%s]: API-Key provider - No matching API Key found for the provided key's hash in '%s' header", p.Name(), p.apiKeyHeaderName)
				http.Error(w, "Unauthorized: Invalid API Key", http.StatusUnauthorized)
				return
			}

			// 3. Compare the incoming plaintext key with the stored bcrypt hash
			err := bcrypt.CompareHashAndPassword(apiKeyDetails.BcryptHash, []byte(incomingPlaintextKey))
			if err != nil { // If err is not nil, the comparison failed (hashes don't match or other bcrypt error)
				logging.Warnf("Plugin [%s]: API-Key provider - API Key bcrypt comparison failed for ClientID '%s': %v", p.Name(), apiKeyDetails.ClientID, err)
				http.Error(w, "Unauthorized: Invalid API Key", http.StatusUnauthorized)
				return
			}

			logging.Debugf("Plugin [%s]: API-Key provider - Valid API Key received for ClientID: '%s'", p.Name(), apiKeyDetails.ClientID)
			r = r.WithContext(context.WithValue(r.Context(), UserContextKey, AuthenticatedUser{ID: apiKeyDetails.ClientID, Claims: nil /* API Keys don't typically have JWT-style claims */}))

		case "none":
			// No authentication required, just pass through.
			logging.Debugf("Plugin [%s]: Provider is 'none', allowing request through.", p.Name())

		default:
			// Unknown provider, could be a configuration error.
			// For security, might be best to deny access or log a critical error.
			logging.Errorf("Plugin [%s]: Unknown authentication provider '%s'. Denying access.", p.Name(), p.Provider)
			http.Error(w, "Internal Server Error: Authentication misconfiguration", http.StatusInternalServerError)
			return // Short-circuit
		}

		// If authentication was successful or not required, call the next handler.
		next.ServeHTTP(w, r)

	})

}

// Shutdown gracefully disconnects the MongoDB client if it was initialized.
func (p *AuthenticationPlugin) Shutdown() error {
	// Stop the periodic refresher first
	if p.apiKeyRefreshTicker != nil {
		close(p.stopAPIKeyRefresh) // Signal the goroutine to stop
	}

	if p.mongoClient != nil {
		logging.Infof("Plugin [%s]: Shutting down MongoDB client...", p.Name())
		ctx, cancel := context.WithTimeout(context.Background(), p.mongoOperationContextTimeout) // Use a timeout for disconnection
		defer cancel()
		err := p.mongoClient.Disconnect(ctx)
		if err != nil {
			logging.Errorf("Plugin [%s]: Error disconnecting from MongoDB: %v", p.Name(), err)
			return err
		}
		logging.Infof("Plugin [%s]: MongoDB client disconnected.", p.Name())
	}
	return nil
}

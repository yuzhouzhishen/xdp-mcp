package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddr     string `yaml:"listen_addr"`
	MQTTBrokerURL  string `yaml:"mqtt_broker_url"`
	MQTTClientID   string `yaml:"mqtt_client_id"`
	MQTTCAFile     string `yaml:"mqtt_ca_file"`
	MQTTCertFile   string `yaml:"mqtt_cert_file"`
	MQTTKeyFile    string `yaml:"mqtt_key_file"`
	DatabaseURL    string `yaml:"database_url"`
	GatewayBaseURL string `yaml:"gateway_base_url"`
}

func (c *Config) LoadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	return nil
}

func (c *Config) MQTTBrokerURLParsed() (*url.URL, error) {
	return url.Parse(c.MQTTBrokerURL)
}

func (c *Config) MQTTTLSConfig() (*tls.Config, error) {
	if strings.HasPrefix(c.MQTTBrokerURL, "mqtt://") {
		return &tls.Config{}, nil
	}

	certpool := x509.NewCertPool()
	if c.MQTTCAFile != "" {
		pemCerts, err := os.ReadFile(c.MQTTCAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		if !certpool.AppendCertsFromPEM(pemCerts) {
			return nil, fmt.Errorf("failed to append CA cert")
		}
	}

	var certs []tls.Certificate
	if c.MQTTCertFile != "" && c.MQTTKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(c.MQTTCertFile, c.MQTTKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load X509 key pair: %w", err)
		}

		cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		certs = append(certs, cert)
	}

	return &tls.Config{
		RootCAs:      certpool,
		ClientAuth:   tls.NoClientCert,
		Certificates: certs,
	}, nil
}

// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package security

import (
	"crypto/tls"

	"github.com/pingcap/errors"
	pd "github.com/pingcap/pd/v4/client"
	"github.com/pingcap/tidb-tools/pkg/utils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Credential holds necessary path parameter to build a tls.Config
type Credential struct {
	CAPath   string `toml:"ca-path" json:"ca-path"`
	CertPath string `toml:"cert-path" json:"cert-path"`
	KeyPath  string `toml:"key-path" json:"key-path"`
}

// PDSecurityOption creates a new pd SecurityOption from Security
func (s *Credential) PDSecurityOption() pd.SecurityOption {
	return pd.SecurityOption{
		CAPath:   s.CAPath,
		CertPath: s.CertPath,
		KeyPath:  s.KeyPath,
	}
}

// ToGRPCDialOption constructs a gRPC dial option.
func (s *Credential) ToGRPCDialOption() (grpc.DialOption, error) {
	tlsCfg, err := s.ToTLSConfig()
	if err != nil || tlsCfg == nil {
		return grpc.WithInsecure(), errors.Trace(err)
	}
	return grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)), nil
}

// ToTLSConfig generates tls's config from *Security
func (s *Credential) ToTLSConfig() (*tls.Config, error) {
	return utils.ToTLSConfig(s.CAPath, s.CertPath, s.KeyPath)
}

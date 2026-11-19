package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/trustedcertificates"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/trustedcertificates/certificateauthority"
)

const certificateAuthorityItem = "Cumulocity CA"

// SyncCertificateAuthority enables or disables the tenant's Cumulocity
// certificate authority. Enabling creates the CA when it does not exist.
// Disabling only sets the status of its trusted certificate to DISABLED: the
// CA is never deleted, since that would invalidate all issued certificates.
func (s *Syncer) SyncCertificateAuthority(ctx context.Context, spec *CertificateAuthoritySpec) error {
	if spec == nil || spec.Enabled == nil {
		return nil
	}
	enabled := *spec.Enabled
	desired := "DISABLED"
	if enabled {
		desired = "ENABLED"
	}
	detail := "status=" + desired
	if spec.AutoRegistrationEnabled != nil {
		detail += fmt.Sprintf(", autoRegistrationEnabled=%t", *spec.AutoRegistrationEnabled)
	}

	if s.DryRun {
		s.record(SectionCertificateAuthority, certificateAuthorityItem, ActionPlanned, detail, nil)
		return nil
	}

	ca := s.Client.TrustedCertificates.CertificateAuthority
	current := ca.Get(ctx, certificateauthority.GetOptions{})
	created := false
	if current.Err != nil {
		if current.HTTPStatus != http.StatusNotFound {
			s.record(SectionCertificateAuthority, certificateAuthorityItem, ActionFailed, "get certificate authority", current.Err)
			return nil
		}
		if !enabled {
			s.record(SectionCertificateAuthority, certificateAuthorityItem, ActionUnchanged, "no certificate authority exists", nil)
			return nil
		}
		if result := ca.Create(ctx, certificateauthority.CreateOptions{}); result.Err != nil {
			s.record(SectionCertificateAuthority, certificateAuthorityItem, ActionFailed, "create certificate authority", result.Err)
			return nil
		}
		created = true
		current = ca.Get(ctx, certificateauthority.GetOptions{})
		if current.Err != nil {
			s.record(SectionCertificateAuthority, certificateAuthorityItem, ActionFailed, "get created certificate authority", current.Err)
			return nil
		}
	}

	cert := current.Data
	upToDate := cert.Status() == desired &&
		(spec.AutoRegistrationEnabled == nil || cert.Get("autoRegistrationEnabled").Bool() == *spec.AutoRegistrationEnabled)
	if upToDate {
		action := ActionUnchanged
		if created {
			action = ActionCreated
		}
		s.record(SectionCertificateAuthority, certificateAuthorityItem, action, detail, nil)
		return nil
	}

	body := map[string]any{"status": desired}
	if spec.AutoRegistrationEnabled != nil {
		body["autoRegistrationEnabled"] = *spec.AutoRegistrationEnabled
	}
	result := s.Client.TrustedCertificates.Update(ctx, trustedcertificates.UpdateOptions{
		Fingerprint: cert.Fingerprint(),
	}, body)
	action := ActionUpdated
	if created {
		action = ActionCreated
	}
	s.record(SectionCertificateAuthority, certificateAuthorityItem, action, detail, result.Err)
	return nil
}

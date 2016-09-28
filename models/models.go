package models

import (
	"database/sql/driver"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/pivotal-cf/brokerapi"
	"github.com/xenolf/lego/acme"

	"github.com/18F/cf-cdn-service-broker/utils"
)

type State string

const (
	Provisioning   State = "provisioning"
	Provisioned          = "provisioned"
	Deprovisioning       = "deprovisioning"
	Deprovisioned        = "deprovisioned"
)

// Marshal a `State` to a `string` when saving to the database
func (s State) Value() (driver.Value, error) {
	return string(s), nil
}

// Unmarshal an `interface{}` to a `State` when reading from the database
func (s *State) Scan(value interface{}) error {
	bytes, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("error scanning status %s", value)
	}
	*s = State(bytes)
	return nil
}

type Route struct {
	gorm.Model
	InstanceId     string `gorm:"not null;unique_index"`
	State          State  `gorm:"not null;index"`
	DomainExternal string
	DomainInternal string
	DistId         string
	Origin         string
	Path           string
	Certificate    Certificate
}

func (r *Route) GetDomains() []string {
	return strings.Split(r.DomainExternal, ",")
}

type Certificate struct {
	gorm.Model
	RouteId     uint
	Domain      string
	CertURL     string
	Certificate []byte
	Expires     time.Time `gorm:"index"`
}

func (c Certificate) Resource() acme.CertificateResource {
	return acme.CertificateResource{
		Domain:      c.Domain,
		CertURL:     c.CertURL,
		Certificate: c.Certificate,
	}
}

type RouteManagerIface interface {
	Create(instanceId, domain, origin, path string) (Route, error)
	Get(instanceId string) (Route, error)
	Update(route Route) error
	Disable(route Route) error
	Renew(route Route) error
	RenewAll()
}

type RouteManager struct {
	Iam        utils.IamIface
	CloudFront utils.DistributionIface
	Acme       utils.AcmeIface
	DB         *gorm.DB
}

func (m *RouteManager) Create(instanceId, domain, origin, path string) (Route, error) {
	route := Route{
		InstanceId:     instanceId,
		State:          Provisioning,
		DomainExternal: domain,
		Origin:         origin,
		Path:           path,
	}

	dist, err := m.CloudFront.Create(route.GetDomains(), origin, path)
	if err != nil {
		return Route{}, err
	}

	route.DomainInternal = *dist.DomainName
	route.DistId = *dist.Id

	m.DB.Create(&route)
	return route, nil
}

func (m *RouteManager) Get(instanceId string) (Route, error) {
	route := Route{}
	result := m.DB.First(&route, Route{InstanceId: instanceId})
	if result.Error == nil {
		return route, nil
	} else if result.RecordNotFound() {
		return Route{}, brokerapi.ErrInstanceDoesNotExist
	} else {
		return Route{}, result.Error
	}
}

func (m *RouteManager) Update(r Route) error {
	switch r.State {
	case Provisioning:
		return m.updateProvisioning(r)
	case Deprovisioning:
		return m.updateDeprovisioning(r)
	default:
		return nil
	}
}

func (m *RouteManager) Disable(r Route) error {
	err := m.CloudFront.Disable(r.DistId)
	if err != nil {
		return err
	}

	r.State = Deprovisioning
	m.DB.Save(&r)

	return nil
}

func (m *RouteManager) Renew(r Route) error {
	var certRow Certificate

	m.DB.Model(&r).Related(&certRow, "Certificate")

	certResource, err := m.Acme.RenewCertificate(certRow.Resource())
	if err != nil {
		return err
	}

	err = m.deployCertificate(r.DomainExternal, r.DistId, certResource)
	if err != nil {
		return err
	}

	expires, err := acme.GetPEMCertExpiration(certResource.Certificate)
	if err != nil {
		return err
	}

	certRow.Domain = certResource.Domain
	certRow.CertURL = certResource.CertURL
	certRow.Certificate = certResource.Certificate
	certRow.Expires = expires
	m.DB.Save(&certRow)

	return nil
}

func (m *RouteManager) RenewAll() {
	routes := []Route{}

	m.DB.Where(
		"state = ? and expires < now() + interval '30 days'", string(Provisioned),
	).Joins(
		"join certificates on routes.id = certificates.route_id",
	).Preload(
		"Certificate",
	).Find(&routes)

	for _, route := range routes {
		m.Renew(route)
	}
}

func (m *RouteManager) updateProvisioning(r Route) error {
	if (m.checkCNAME(r) || m.checkHosts(r)) && m.checkDistribution(r) {
		certResource, err := m.provisionCert(r)
		if err != nil {
			return err
		}

		expires, err := acme.GetPEMCertExpiration(certResource.Certificate)
		if err != nil {
			return err
		}

		certRow := Certificate{
			Domain:      certResource.Domain,
			CertURL:     certResource.CertURL,
			Certificate: certResource.Certificate,
			Expires:     expires,
		}
		m.DB.Create(&certRow)

		r.State = Provisioned
		r.Certificate = certRow
		m.DB.Save(&r)
	}

	return nil
}

func (m *RouteManager) updateDeprovisioning(r Route) error {
	deleted, err := m.CloudFront.Delete(r.DistId)
	if err != nil {
		return err
	}

	if deleted {
		err = m.Iam.DeleteCertificate(fmt.Sprintf("cdn-route-%s", r.DomainExternal), false)
		if err != nil {
			return err
		}

		r.State = Deprovisioned
		m.DB.Save(&r)
	}

	return nil
}

func (m *RouteManager) provisionCert(r Route) (acme.CertificateResource, error) {
	cert, err := m.Acme.ObtainCertificate(r.GetDomains())
	if err != nil {
		return acme.CertificateResource{}, err
	}

	err = m.deployCertificate(r.DomainExternal, r.DistId, cert)
	if err != nil {
		return acme.CertificateResource{}, err
	}

	return cert, nil
}

func (m *RouteManager) checkCNAME(r Route) bool {
	expects := fmt.Sprintf("%s.", r.DomainInternal)

	for _, d := range r.GetDomains() {
		cname, err := net.LookupCNAME(d)
		if err != nil || cname != expects {
			return false
		}
	}

	return true
}

func (m *RouteManager) checkHosts(r Route) bool {
	hosts, err := net.LookupHost(r.DomainInternal)
	if err != nil {
		return false
	}
	sort.Strings(hosts)

	for _, d := range r.GetDomains() {
		obsHosts, err := net.LookupHost(d)
		if err != nil {
			return false
		}
		sort.Strings(obsHosts)
		if !reflect.DeepEqual(hosts, obsHosts) {
			return false
		}
	}

	return true
}

func (m *RouteManager) checkDistribution(r Route) bool {
	dist, err := m.CloudFront.Get(r.DistId)
	if err != nil {
		return false
	}

	return *dist.Status == "Deployed" && *dist.DistributionConfig.Enabled
}

func (m *RouteManager) deployCertificate(domain, distId string, cert acme.CertificateResource) error {
	prev := fmt.Sprintf("cdn-route-%s-new", domain)
	next := fmt.Sprintf("cdn-route-%s", domain)

	certId, err := m.Iam.UploadCertificate(prev, cert)
	if err != nil {
		return err
	}

	err = m.CloudFront.SetCertificate(distId, certId)
	if err != nil {
		return err
	}

	return m.Iam.RenameCertificate(prev, next)
}

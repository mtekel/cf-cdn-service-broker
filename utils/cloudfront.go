package utils

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudfront"

	"github.com/18F/cf-cdn-service-broker/config"
)

type DistributionIface interface {
	Create(domains []string, origin, path string, insecureOrigin bool, forwardedHeaders []string, tags map[string]string) (*cloudfront.Distribution, error)
	Update(distId string, domains []string, origin, path string, insecureOrigin bool, forwardedHeaders []string) (*cloudfront.Distribution, error)
	Get(distId string) (*cloudfront.Distribution, error)
	SetCertificate(distId, certId string) error
	Disable(distId string) error
	Delete(distId string) (bool, error)
}

type Distribution struct {
	Settings config.Settings
	Service  *cloudfront.CloudFront
}

func (d *Distribution) getDistributionId(domains []string) string {
	return fmt.Sprintf("%scdn-route-%s", d.Settings.CloudFrontPrefix, strings.Join(domains, ":"))
}

func (d *Distribution) getOriginId(domains []string) string {
	return fmt.Sprintf("cdn-route-%s", strings.Join(domains, ":"))
}

func (d *Distribution) getAliases(domains []string) *cloudfront.Aliases {
	var items []*string
	for _, d := range domains {
		items = append(items, aws.String(d))
	}
	return &cloudfront.Aliases{
		Quantity: aws.Int64(int64(len(domains))),
		Items:    items,
	}
}

func (d *Distribution) getTags(tags map[string]string) *cloudfront.Tags {
	items := []*cloudfront.Tag{}
	for key, value := range tags {
		items = append(items, &cloudfront.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}
	return &cloudfront.Tags{Items: items}
}

func (d *Distribution) getHeaders(headers []string) *cloudfront.Headers {
	items := make([]*string, len(headers))
	for idx, header := range headers {
		items[idx] = aws.String(header)
	}
	return &cloudfront.Headers{
		Quantity: aws.Int64(int64(len(headers))),
		Items:    items,
	}
}

// fillDistributionConfig is a wrapper function that will get all the common config settings for
// "cloudfront.DistributionConfig". This function is shared between "Create" and "Update".
// In order to maintain backwards compatibility with older versions of the code where the callerReference was derived
// from the domain(s), the callerReference has to be explicitly passed in. This is necessary because whenever we do an
// update, the domains could change but we need to treat the CallerReference like an ID because
// it can't be changed like the domains and instead the callerReference which was composed of the original domains must
// be passed in.
func (d *Distribution) fillDistributionConfig(config *cloudfront.DistributionConfig, origin, path string,
	insecureOrigin bool, callerReference *string, domains []string, forwardedHeaders []string) {
	config.CallerReference = callerReference
	config.Comment = aws.String("cdn route service")
	config.Enabled = aws.Bool(true)
	config.IsIPV6Enabled = aws.Bool(true)
	config.DefaultCacheBehavior = &cloudfront.DefaultCacheBehavior{
		TargetOriginId: aws.String(d.getOriginId(domains)),
		ForwardedValues: &cloudfront.ForwardedValues{
			Headers: d.getHeaders(forwardedHeaders),
			Cookies: &cloudfront.CookiePreference{
				Forward: aws.String("all"),
			},
			QueryString: aws.Bool(true),
			QueryStringCacheKeys: &cloudfront.QueryStringCacheKeys{
				Quantity: aws.Int64(0),
			},
		},
		SmoothStreaming: aws.Bool(false),
		DefaultTTL:      aws.Int64(86400),
		MinTTL:          aws.Int64(0),
		MaxTTL:          aws.Int64(31536000),
		LambdaFunctionAssociations: &cloudfront.LambdaFunctionAssociations{
			Quantity: aws.Int64(0),
		},
		TrustedSigners: &cloudfront.TrustedSigners{
			Enabled:  aws.Bool(false),
			Quantity: aws.Int64(0),
		},
		ViewerProtocolPolicy: aws.String("redirect-to-https"),
		AllowedMethods: &cloudfront.AllowedMethods{
			CachedMethods: &cloudfront.CachedMethods{
				Quantity: aws.Int64(2),
				Items: []*string{
					aws.String("HEAD"),
					aws.String("GET"),
				},
			},
			Quantity: aws.Int64(7),
			Items: []*string{
				aws.String("HEAD"),
				aws.String("GET"),
				aws.String("OPTIONS"),
				aws.String("PUT"),
				aws.String("POST"),
				aws.String("PATCH"),
				aws.String("DELETE"),
			},
		},
		Compress: aws.Bool(false),
	}
	config.Origins = &cloudfront.Origins{
		Quantity: aws.Int64(2),
		Items: []*cloudfront.Origin{
			{
				DomainName: aws.String(origin),
				Id:         aws.String(d.getOriginId(domains)),
				OriginPath: aws.String(path),
				CustomHeaders: &cloudfront.CustomHeaders{
					Quantity: aws.Int64(0),
				},
				CustomOriginConfig: &cloudfront.CustomOriginConfig{
					HTTPPort:             aws.Int64(80),
					HTTPSPort:            aws.Int64(443),
					OriginProtocolPolicy: getOriginProtocolPolicy(insecureOrigin),
					OriginSslProtocols: &cloudfront.OriginSslProtocols{
						Quantity: aws.Int64(3),
						Items: []*string{
							aws.String("TLSv1"),
							aws.String("TLSv1.1"),
							aws.String("TLSv1.2"),
						},
					},
				},
			},
			{
				DomainName: aws.String(fmt.Sprintf("%s.s3.amazonaws.com", d.Settings.Bucket)),
				Id:         aws.String(fmt.Sprintf("s3-%s-%s", d.Settings.Bucket, domains)),
				OriginPath: aws.String(""),
				CustomHeaders: &cloudfront.CustomHeaders{
					Quantity: aws.Int64(0),
				},
				S3OriginConfig: &cloudfront.S3OriginConfig{
					OriginAccessIdentity: aws.String(""),
				},
			},
		},
	}
	config.CacheBehaviors = &cloudfront.CacheBehaviors{
		Quantity: aws.Int64(1),
		Items: []*cloudfront.CacheBehavior{
			{
				AllowedMethods: &cloudfront.AllowedMethods{
					CachedMethods: &cloudfront.CachedMethods{
						Quantity: aws.Int64(2),
						Items: []*string{
							aws.String("HEAD"),
							aws.String("GET"),
						},
					},
					Items: []*string{
						aws.String("HEAD"),
						aws.String("GET"),
					},
					Quantity: aws.Int64(2),
				},
				Compress:       aws.Bool(false),
				PathPattern:    aws.String("/.well-known/acme-challenge/*"),
				TargetOriginId: aws.String(fmt.Sprintf("s3-%s-%s", d.Settings.Bucket, domains)),
				ForwardedValues: &cloudfront.ForwardedValues{
					Headers: &cloudfront.Headers{
						Quantity: aws.Int64(0),
					},
					QueryString: aws.Bool(false),
					Cookies: &cloudfront.CookiePreference{
						Forward: aws.String("none"),
					},
					QueryStringCacheKeys: &cloudfront.QueryStringCacheKeys{
						Quantity: aws.Int64(0),
					},
				},
				SmoothStreaming: aws.Bool(false),
				DefaultTTL:      aws.Int64(86400),
				MinTTL:          aws.Int64(0),
				MaxTTL:          aws.Int64(31536000),
				LambdaFunctionAssociations: &cloudfront.LambdaFunctionAssociations{
					Quantity: aws.Int64(0),
				},
				TrustedSigners: &cloudfront.TrustedSigners{
					Enabled:  aws.Bool(false),
					Quantity: aws.Int64(0),
				},
				ViewerProtocolPolicy: aws.String("allow-all"),
			},
		},
	}
	config.Aliases = d.getAliases(domains)
	config.PriceClass = aws.String("PriceClass_100")
}

func (d *Distribution) Create(domains []string, origin, path string, insecureOrigin bool, forwardedHeaders []string, tags map[string]string) (*cloudfront.Distribution, error) {
	distConfig := new(cloudfront.DistributionConfig)
	d.fillDistributionConfig(distConfig, origin, path, insecureOrigin,
		aws.String(d.getDistributionId(domains)), domains, forwardedHeaders)
	resp, err := d.Service.CreateDistributionWithTags(&cloudfront.CreateDistributionWithTagsInput{
		DistributionConfigWithTags: &cloudfront.DistributionConfigWithTags{
			DistributionConfig: distConfig,
			Tags:               d.getTags(tags),
		},
	})

	if err != nil {
		return &cloudfront.Distribution{}, err
	}

	return resp.Distribution, nil
}

func (d *Distribution) Update(distId string, domains []string, origin, path string, insecureOrigin bool, forwardedHeaders []string) (*cloudfront.Distribution, error) {
	// Get the current distribution
	dist, err := d.Service.GetDistributionConfig(&cloudfront.GetDistributionConfigInput{
		Id: aws.String(distId),
	})
	if err != nil {
		return nil, err
	}
	d.fillDistributionConfig(dist.DistributionConfig, origin, path, insecureOrigin,
		dist.DistributionConfig.CallerReference, domains, forwardedHeaders)

	// Call the UpdateDistribution function
	resp, err := d.Service.UpdateDistribution(&cloudfront.UpdateDistributionInput{
		Id:                 aws.String(distId),
		IfMatch:            dist.ETag,
		DistributionConfig: dist.DistributionConfig,
	})
	if err != nil {
		return &cloudfront.Distribution{}, err
	}
	return resp.Distribution, nil
}

func (d *Distribution) Get(distId string) (*cloudfront.Distribution, error) {
	resp, err := d.Service.GetDistribution(&cloudfront.GetDistributionInput{
		Id: aws.String(distId),
	})
	if err != nil {
		return &cloudfront.Distribution{}, err
	}
	return resp.Distribution, nil
}

func (d *Distribution) SetCertificate(distId, certId string) error {
	resp, err := d.Service.GetDistributionConfig(&cloudfront.GetDistributionConfigInput{
		Id: aws.String(distId),
	})
	if err != nil {
		return err
	}

	DistributionConfig, ETag := resp.DistributionConfig, resp.ETag

	DistributionConfig.ViewerCertificate.Certificate = aws.String(certId)
	DistributionConfig.ViewerCertificate.IAMCertificateId = aws.String(certId)
	DistributionConfig.ViewerCertificate.CertificateSource = aws.String("iam")
	DistributionConfig.ViewerCertificate.SSLSupportMethod = aws.String("sni-only")
	DistributionConfig.ViewerCertificate.MinimumProtocolVersion = aws.String("TLSv1")
	DistributionConfig.ViewerCertificate.CloudFrontDefaultCertificate = aws.Bool(false)

	_, err = d.Service.UpdateDistribution(&cloudfront.UpdateDistributionInput{
		Id:                 aws.String(distId),
		IfMatch:            ETag,
		DistributionConfig: DistributionConfig,
	})

	return err
}

func (d *Distribution) Disable(distId string) error {
	resp, err := d.Service.GetDistributionConfig(&cloudfront.GetDistributionConfigInput{
		Id: aws.String(distId),
	})
	if err != nil {
		return err
	}

	DistributionConfig, ETag := resp.DistributionConfig, resp.ETag
	DistributionConfig.Enabled = aws.Bool(false)

	_, err = d.Service.UpdateDistribution(&cloudfront.UpdateDistributionInput{
		Id:                 aws.String(distId),
		IfMatch:            ETag,
		DistributionConfig: DistributionConfig,
	})

	return err
}

func (d *Distribution) Delete(distId string) (bool, error) {
	resp, err := d.Service.GetDistribution(&cloudfront.GetDistributionInput{
		Id: aws.String(distId),
	})
	if err != nil {
		return false, err
	}

	if *resp.Distribution.Status != "Deployed" {
		return false, nil
	}

	_, err = d.Service.DeleteDistribution(&cloudfront.DeleteDistributionInput{
		Id:      aws.String(distId),
		IfMatch: resp.ETag,
	})

	return err == nil, err
}

func getOriginProtocolPolicy(insecure bool) *string {
	if insecure {
		return aws.String("http-only")
	}
	return aws.String("https-only")
}

package main

import (
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/go-co-op/gocron"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	RecordType     = "A"
	FQDNEnvVar     = "CONFIG_R53DDNS_HOSTNAME"
	PublicIPURL    = "CONFIG_R53DDNS_IPURL"
	TTL            = 300
	UpdateInterval = 300
)

var (
	// DomainRegex \x2E regex is equal to a literal period `.`
	domainRegex = regexp.MustCompile(`^([^\x2E]*)\x2E(.*)$`)
	scheduler   *gocron.Scheduler
	awsSession  *session.Session
	dnsClient   *route53.Route53
	fqdn        string
	ipURL       string
)

func init() {
	// initialize hostname
	fqdn = os.Getenv(FQDNEnvVar)
	if fqdn == "" {
		log.Fatalf("%s %s", FQDNEnvVar, "environmental variable is not set")
	}

	// initialize public ip address URL
	ipURL = os.Getenv(PublicIPURL)
	if ipURL == "" {
		log.Fatalf("%s %s", PublicIPURL, "environmental variable is not set")
	}

	// create cron scheduler
	scheduler = gocron.NewScheduler(time.UTC)

	// create AWS session
	awsSession = session.Must(session.NewSession())

	// create a Route53 client
	dnsClient = route53.New(awsSession, aws.NewConfig())
}

func main() {
	_, err := scheduler.Every(UpdateInterval).Seconds().Do(getIPAndUpdate)
	if err != nil {
		log.Printf("%s: %v", "failure setting up job", err)
	}

	scheduler.StartBlocking()
}

func getIPAndUpdate() error {
	// retrieve current ip address
	ip, err := getIP()
	if err != nil {
		return errors.New(fmt.Sprintf("%s: %v", "unable to determine ip address", err))
	}

	// create or update record
	if err := upsertRoute53Record(ip, fqdn, dnsClient); err != nil {
		return errors.New(fmt.Sprintf("%s: %v", "could not update record", err))
	}

	return nil
}

func getIP() (string, error) {
	resp, err := http.Get(ipURL)
	if err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Printf("%s: %v", "unable to close http socket", err)
		}
	}(resp.Body)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	formatted := strings.TrimSuffix(string(body), "\n")

	// ensure it is an ip
	ip := net.ParseIP(formatted)
	if ip == nil {
		return "", errors.New(fmt.Sprintf("%s: %s", "not a valid IP address", ip))
	}

	return ip.String(), nil
}

func upsertRoute53Record(ip, fqdn string, dnsClient *route53.Route53) error {
	// extract domain
	tokens := domainRegex.FindStringSubmatch(fqdn)
	domain := tokens[2]

	// http://docs.aws.amazon.com/sdk-for-go/api/service/route53/Route53.html#ListHostedZonesByName-instance_method
	resources, err := dnsClient.ListHostedZonesByName(&route53.ListHostedZonesByNameInput{
		DNSName:  aws.String(domain + "."),
		MaxItems: aws.String("1"),
	})

	if err != nil {
		return err
	}

	// validation
	if len(resources.HostedZones) != 1 {
		return errors.New(fmt.Sprintf("%s (%s): %v\n", "could not find domain", domain, err))
	}
	if *resources.DNSName != domain+"." {
		return errors.New(fmt.Sprintf("%s - %s)\n", domain, *resources.DNSName))
	}

	// extract zone ID from resources
	zoneIDTokens := strings.Split(*resources.HostedZones[0].Id, "/")
	zoneID := zoneIDTokens[len(zoneIDTokens)-1]

	// list records
	resp, err := dnsClient.ListResourceRecordSets(&route53.ListResourceRecordSetsInput{
		StartRecordName: aws.String(fqdn),
		StartRecordType: aws.String(RecordType),
		HostedZoneId:    aws.String(zoneID),
		MaxItems:        aws.String("1"),
	})
	if err != nil {
		return errors.New(fmt.Sprintf("%s (%s): %v\n", "error listing records", domain, err))
	}

	var foundResource bool
	if len(resp.ResourceRecordSets) != 1 {
		foundResource = false
	} else {
		foundResource = *resp.ResourceRecordSets[0].Name == fqdn+"."
		if foundResource {
			for _, record := range resp.ResourceRecordSets[0].ResourceRecords {
				if *record.Value == ip {
					log.Printf("%s already registered in route53 as %s\n", ip, fqdn)
					return nil
				}
			}
		}
	}

	// initialize A record
	resourceRecordSet := &route53.ResourceRecordSet{
		Name: aws.String(fqdn + "."),
		Type: aws.String("A"),
		ResourceRecords: []*route53.ResourceRecord{
			{
				Value: aws.String(ip),
			},
		},
		TTL: aws.Int64(TTL),
	}

	// use upsert action
	upsert := []*route53.Change{{
		Action:            aws.String("UPSERT"),
		ResourceRecordSet: resourceRecordSet,
	}}

	// set params for the upsert and zoneID
	params := route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: upsert,
		},
		HostedZoneId: aws.String(zoneID),
	}

	// attempt change
	_, err = dnsClient.ChangeResourceRecordSets(&params)

	if err != nil {
		return errors.New(fmt.Sprintf("%s: %v\n", "failed to update record set", err))
	}

	log.Printf("submitted change for zone ID %s to register %s as %s\n", zoneID, ip, fqdn)

	return nil
}

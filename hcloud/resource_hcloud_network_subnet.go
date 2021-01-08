package hcloud

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hetznercloud/hcloud-go/hcloud"
)

func resourceNetworkSubnet() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceNetworkSubnetCreate,
		ReadContext:   resourceNetworkSubnetRead,
		DeleteContext: resourceNetworkSubnetDelete,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Schema: map[string]*schema.Schema{
			"network_id": {
				Type:     schema.TypeInt,
				Required: true,
				ForceNew: true,
			},
			"type": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
				ValidateFunc: validation.StringInSlice([]string{
					"cloud",
					"server",
					"vswitch",
				}, false),
			},
			"network_zone": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"ip_range": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"gateway": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"vswitch_id": {
				Type:     schema.TypeInt,
				Optional: true,
				ForceNew: true,
			},
		},
	}
}

func resourceNetworkSubnetCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	var action *hcloud.Action

	client := m.(*hcloud.Client)

	_, ipRange, err := net.ParseCIDR(d.Get("ip_range").(string))
	if err != nil {
		return diag.FromErr(err)
	}
	networkID := d.Get("network_id")
	network := &hcloud.Network{ID: networkID.(int)}

	subnetType := hcloud.NetworkSubnetType(d.Get("type").(string))
	opts := hcloud.NetworkAddSubnetOpts{
		Subnet: hcloud.NetworkSubnet{
			IPRange:     ipRange,
			NetworkZone: hcloud.NetworkZone(d.Get("network_zone").(string)),
			Type:        subnetType,
		},
	}

	if subnetType == hcloud.NetworkSubnetTypeVSwitch {
		vSwitchID := d.Get("vswitch_id")
		opts.Subnet.VSwitchID = vSwitchID.(int)
	}

	err = retry(defaultMaxRetries, func() error {
		var err error

		action, _, err = client.Network.AddSubnet(ctx, network, opts)
		if hcloud.IsError(err, hcloud.ErrorCodeConflict) {
			return err
		}
		return abortRetry(err)
	})
	if err != nil {
		return diag.FromErr(err)
	}

	if err := waitForNetworkAction(ctx, client, action, network); err != nil {
		return diag.FromErr(err)
	}
	d.SetId(generateNetworkSubnetID(network, ipRange.String()))

	return resourceNetworkSubnetRead(ctx, d, m)
}

func resourceNetworkSubnetRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*hcloud.Client)

	network, subnet, err := lookupNetworkSubnetID(ctx, d.Id(), client)
	if err == errInvalidNetworkSubnetID {
		log.Printf("[WARN] Invalid id (%s), removing from state: %s", d.Id(), err)
		d.SetId("")
		return nil
	}
	if err != nil {
		return diag.FromErr(err)
	}
	if network == nil {
		log.Printf("[WARN] Network Subnet (%s) not found, removing from state", d.Id())
		d.SetId("")
		return nil
	}
	d.SetId(generateNetworkSubnetID(network, subnet.IPRange.String()))
	setNetworkSubnetSchema(d, network, subnet)
	return nil

}

func resourceNetworkSubnetDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	var action *hcloud.Action

	client := m.(*hcloud.Client)

	network, subnet, err := lookupNetworkSubnetID(ctx, d.Id(), client)

	if err != nil {
		log.Printf("[WARN] Invalid id (%s), removing from state: %s", d.Id(), err)
		d.SetId("")
		return nil
	}
	err = retry(defaultMaxRetries, func() error {
		var err error

		action, _, err = client.Network.DeleteSubnet(ctx, network, hcloud.NetworkDeleteSubnetOpts{
			Subnet: subnet,
		})
		if hcloud.IsError(err, hcloud.ErrorCodeConflict) || hcloud.IsError(err, hcloud.ErrorCodeLocked) {
			return err
		}
		if hcloud.IsError(err, hcloud.ErrorCodeServiceError) &&
			err.Error() == "cannot remove subnet because servers are attached to it (service_error)" {
			return err
		}
		return abortRetry(err)
	})
	if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		// network subnet has already been deleted
		return nil
	}
	if err != nil {
		return diag.FromErr(err)
	}
	if err := waitForNetworkAction(ctx, client, action, network); err != nil {
		return diag.FromErr(err)
	}
	return nil
}

func setNetworkSubnetSchema(d *schema.ResourceData, n *hcloud.Network, s hcloud.NetworkSubnet) {
	d.SetId(generateNetworkSubnetID(n, s.IPRange.String()))
	d.Set("network_id", n.ID)
	d.Set("network_zone", s.NetworkZone)
	d.Set("ip_range", s.IPRange.String())
	d.Set("type", s.Type)
	d.Set("gateway", s.Gateway.String())
	if s.Type == hcloud.NetworkSubnetTypeVSwitch {
		d.Set("vswitch_id", s.VSwitchID)
	}
}

func generateNetworkSubnetID(network *hcloud.Network, ipRange string) string {
	return fmt.Sprintf("%d-%s", network.ID, ipRange)
}

func parseNetworkSubnetID(s string) (int, *net.IPNet, error) {
	if s == "" {
		return 0, nil, errInvalidNetworkSubnetID
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, nil, errInvalidNetworkSubnetID
	}

	networkID, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, nil, errInvalidNetworkSubnetID
	}

	_, ipRange, err := net.ParseCIDR(parts[1])
	if ipRange == nil || err != nil {
		return 0, nil, errInvalidNetworkSubnetID
	}

	return networkID, ipRange, nil
}

var errInvalidNetworkSubnetID = errors.New("invalid network subnet id")

// lookupNetworkSubnetID parses the terraform network subnet record id and return the network and subnet
//
// id format: <network id>-<ip range>
// Examples:
// 123-192.168.100.1/32 (network subnet of network 123 with the ip range 192.168.100.1/32)
func lookupNetworkSubnetID(ctx context.Context, terraformID string, client *hcloud.Client) (*hcloud.Network, hcloud.NetworkSubnet, error) {
	networkID, ipRange, err := parseNetworkSubnetID(terraformID)
	if err != nil {
		return nil, hcloud.NetworkSubnet{}, err
	}
	network, _, err := client.Network.GetByID(ctx, networkID)
	if err != nil {
		return nil, hcloud.NetworkSubnet{}, err
	}
	if network == nil {
		return nil, hcloud.NetworkSubnet{}, errInvalidNetworkSubnetID
	}
	for _, sn := range network.Subnets {
		if sn.IPRange.String() == ipRange.String() {
			return network, sn, nil
		}
	}
	return nil, hcloud.NetworkSubnet{}, nil
}

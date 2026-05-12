package userApiGolang

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
)

var URL = "https://proxy-seller.com/personal/api/v1/"

var apiKey string
var paymentId int
var generateAuth string

type ResultData struct {
	Slice []interface{}
	Map   map[string]interface{}
}

func NewResultData() ResultData {
	return ResultData{
		Slice: nil,
		Map:   nil,
	}
}

// SetApiKey Key placed in https://proxy-seller.com/personal/api/
func SetApiKey(key string) {
	apiKey = key
}

// SetPaymentId payment id=1(inner balance), id=43(subscribed card)
func SetPaymentId(id int) {
	paymentId = id
}

func GetPaymentId() int {
	return paymentId
}

// SetGenerateAuth Y or N
func SetGenerateAuth(yn string) {
	if yn == "Y" {
		generateAuth = "Y"
	} else {
		generateAuth = "N"
	}
}

func GetGenerateAuth() string {
	return generateAuth
}

func checkApiKey() {
	if apiKey == "" {
		log.Fatal("Need key, placed in https://proxy-seller.com/personal/api/")
	}
}

func tryConvert(i interface{}) ResultData {
	result := NewResultData()
	// Попытка преобразования в map[string]interface{}
	if m, ok := i.(map[string]interface{}); ok {
		//fmt.Println("Value is a map[string]interface{}:", m)
		result.Map = m
		return result
	}

	// Попытка преобразования в []interface{}
	if s, ok := i.([]interface{}); ok {
		//fmt.Println("Value is a []interface{}:", s)
		result.Slice = s
		return result
	}

	return result
}

// Request Send request into server
func Request(method string, uri string, data map[string]interface{}) (ResultData, error) {
	checkApiKey()

	//log.Println(URL + apiKey + "/" + uri)
	var resp *http.Response
	var err error

	if method == "GET" {
		resp, err = http.Get(URL + apiKey + "/" + uri)
	} else {
		//log.Println(data)
		bytesRepresentation, _ := json.Marshal(data)
		client := &http.Client{}
		req, err := http.NewRequest(method, URL+apiKey+"/"+uri, bytes.NewBuffer(bytesRepresentation))
		req.Header.Set("Content-Type", "application/json")
		if err != nil {
			return NewResultData(), err
		}
		// send the request
		resp, err = client.Do(req)
		if err != nil {
			return NewResultData(), err
		}
		defer func(Body io.ReadCloser) {
			err := Body.Close()
			if err != nil {

			}
		}(resp.Body)
	}

	var result map[string]interface{}
	if err != nil {
		log.Fatal(err)
	} else {
		//fmt.Println(resp)
		err = json.NewDecoder(resp.Body).Decode(&result)
		if err != nil {
			return NewResultData(), err
		}
		//log.Println(result)
		if result["status"] == "success" {
			//log.Println(result["data"])
			return tryConvert(result["data"]), nil
		} else {
			log.Println(result["errors"])
			return NewResultData(), fmt.Errorf("%g", result["errors"])
		}
	}
	return NewResultData(), nil
}

func RequestBinary(uri string) []byte {
	checkApiKey()

	resp, err := http.Get(URL + apiKey + "/" + uri)
	if err != nil {
		fmt.Printf("Error fetching file: %v\n", err)
		return nil
	}
	defer resp.Body.Close() // Ensure the response body is closed

	// Check if the HTTP request was successful
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Server returned non-200 status: %d %s\n", resp.StatusCode, resp.Status)
		return nil
	}

	// Read the response body
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading response body: %v\n", err)
		return nil
	}

	return data
}

/////////////////////////////// Auth ///////////////////////////////

/**
 * TODO: Get auths
 * @return array Returns list auths
 */
func authList() (map[string]interface{}, error) {
	result, err := Request("GET", "auth/list", nil)
	// TODO:
	//if result["items"] != "" {
	//	return result["items"].([]interface{})
	//}
	return result.Map, err
}

/**
 * TODO: Set auth active state
 * @param integer $id
 * @param string $active
 * @return string Returns current auth
 */
func authActive(id int, active string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"id":     id,
		"active": active,
	}
	result, err := Request("POST", "auth/active", data)
	return result.Map, err
}

/////////////////////////////// Balance ///////////////////////////////

// Balance Get balance statistic
func Balance() float64 {
	result, err := Request("GET", "balance/get", nil)
	if err != nil {
		return -1
	} else {
		summ, ok := result.Map["summ"].(float64)
		if !ok {
			return -1
		}
		return summ
	}
}

// BalanceAdd Replenish the balance
// Returns a link to the payment page
// https://proxy-seller.com/personal/pay/?ORDER_ID=123456789&PAYMENT_ID=987654321&HASH=343bd596fb97c04bfb76557710837d34
func BalanceAdd(summ float64, paymentId int) string {
	data := map[string]interface{}{
		"summ":      summ,
		"paymentId": paymentId,
	}
	result, err := Request("POST", "balance/add", data)
	if err != nil {
		return ""
	} else {
		return result.Map["url"].(string)
	}
}

// BalancePaymentsList List of payment systems for balance replenishing
// return array Example items:
// [
//    [
//       'id' => '29',
//       'name' =>'PayPal'
//    ],
//    [
//      'id' => '37',
//       'name' => 'Visa / MasterCard'
//    ]
// ]
func BalancePaymentsList() ([]interface{}, error) {
	result, err := Request("GET", "balance/payments/list", nil)
	if err != nil {
		return nil, err
	}
	if result.Map["items"] != "" {
		return result.Map["items"].([]interface{}), nil
	}
	return nil, err
}

/////////////////////////////// Order ///////////////////////////////

// ReferenceList Necessary guides for creating an order
// @param string proxyType - ipv4 | ipv6 | mobile | isp | mix | ""
// - Countries + operators and rotation periods (mobile only)
// - Proxy periods
// - Purposes and services (only for ipv4,ipv6,isp,mix,mix_isp,resident)
// - Quantities allowed (only for mix proxy)
func ReferenceList(proxyType string) (map[string]interface{}, error) {
	result, err := Request("GET", "reference/list/"+proxyType, nil)
	return result.Map, err
}

// OrderCalcIpv4 Calculate the order IPv4
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// @param integer countryId
// @param integer periodId
// @param integer quantity
// @param string authorization
// @param string coupon
// @param string customTargetName
// @return array Example
// [
//     'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//     'balance' => 2,
//     'total' => 35.1,
//     'quantity' => 5,
//     'currency' => 'USD',
//     'discount' => 0.22,
//     'price' => 7.02
// ]
func OrderCalcIpv4(countryId int, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return orderCalc(prepareIpv4(countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderCalcIsp Calculate the order ISP
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// @param integer countryId
// @param integer periodId
// @param integer quantity
// @param string authorization
// @param string coupon
// @param string customTargetName
// @return array Example
// [
//     'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//     'balance' => 2,
//     'total' => 35.1,
//     'quantity' => 5,
//     'currency' => 'USD',
//     'discount' => 0.22,
//     'price' => 7.02
// ]
func OrderCalcIsp(countryId int, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return orderCalc(prepareIpv4(countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderCalcMix Calculate the order ISP
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// @param integer countryId
// @param integer periodId
// @param integer quantity
// @param string authorization
// @param string coupon
// @param string customTargetName
// @return array Example
// [
//     'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//     'balance' => 2,
//     'total' => 35.1,
//     'quantity' => 5,
//     'currency' => 'USD',
//     'discount' => 0.22,
//     'price' => 7.02
// ]
func OrderCalcMix(countryId int, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return orderCalc(prepareIpv4(countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderCalcIpv6 Calculate the order IPv6
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// @param integer countryId
// @param integer periodId
// @param integer quantity
// @param string authorization
// @param string coupon
// @param string customTargetName
// @return array Example
// [
//     'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//     'balance' => 2,
//     'total' => 35.1,
//     'quantity' => 5,
//     'currency' => 'USD',
//     'discount' => 0.22,
//     'price' => 7.02
// ]
func OrderCalcIpv6(countryId int, periodId string, quantity int, authorization string, coupon string, customTargetName string, protocol string) (map[string]interface{}, error) {
	return orderCalc(prepareIpv6(countryId, periodId, quantity, authorization, coupon, customTargetName, protocol))
}

// OrderCalcMobile Calculate the order IPv6
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// @param integer countryId
// @param integer periodId
// @param integer quantity
// @param string authorization
// @param string coupon
// @param string customTargetName
// @return array Example
// [
//     'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//     'balance' => 2,
//     'total' => 35.1,
//     'quantity' => 5,
//     'currency' => 'USD',
//     'discount' => 0.22,
//     'price' => 7.02
// ]
func OrderCalcMobile(countryId int, periodId string, quantity int, authorization string, coupon string, operatorId string, rotationId string) (map[string]interface{}, error) {
	return orderCalc(prepareIpv6(countryId, periodId, quantity, authorization, coupon, operatorId, rotationId))
}

// OrderCalcResident Calculate the order Resident
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// @param integer tarifId
// @param string coupon
// @return array Example
// [
//     'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//     'balance' => 2,
//     'total' => 35.1,
//     'quantity' => 5,
//     'currency' => 'USD',
//     'discount' => 0.22,
//     'price' => 7.02
// ]
func OrderCalcResident(tarifId int, coupon string) (map[string]interface{}, error) {
	return orderCalc(prepareResident(tarifId, coupon))
}

// OrderMakeIpv4 Create an order IPv4
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param integer countryId
// @param integer periodId
// @param integer quantity
// @param string authorization
// @param string coupon
// @param string customTargetName
// @return array Example
// [
//     'orderId' => 1000000,
//     'total' => 35.1,
//     'balance' => 10.19
// ]
func OrderMakeIpv4(countryId int, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return orderMake(prepareIpv4(countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderMakeIsp Create an order ISP
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param integer countryId
// @param integer periodId
// @param integer quantity
// @param string authorization
// @param string coupon
// @param string customTargetName
// @return array Example
// [
//     'orderId' => 1000000,
//     'total' => 35.1,
//     'balance' => 10.19
// ]
func OrderMakeIsp(countryId int, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return orderMake(prepareIpv4(countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderMakeMix Create an order Mix
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param integer countryId
// @param integer periodId
// @param integer quantity
// @param string authorization
// @param string coupon
// @param string customTargetName
// @return array Example
// [
//     'orderId' => 1000000,
//     'total' => 35.1,
//     'balance' => 10.19
// ]
func OrderMakeMix(countryId int, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return orderMake(prepareIpv4(countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderMakeIpv6 Create an order IPv6
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param integer countryId
// @param integer periodId
// @param integer quantity
// @param string authorization
// @param string coupon
// @param string customTargetName
// @return array Example
// [
//     'orderId' => 1000000,
//     'total' => 35.1,
//     'balance' => 10.19
// ]
func OrderMakeIpv6(countryId int, periodId string, quantity int, authorization string, coupon string, customTargetName string, protocol string) (map[string]interface{}, error) {
	return orderMake(prepareIpv6(countryId, periodId, quantity, authorization, coupon, customTargetName, protocol))
}

// OrderMakeMobile Create an order Mobile
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param integer countryId
// @param integer periodId
// @param integer quantity
// @param string authorization
// @param string coupon
// @param string customTargetName
// @return array Example
// [
//     'orderId' => 1000000,
//     'total' => 35.1,
//     'balance' => 10.19
// ]
func OrderMakeMobile(countryId int, periodId string, quantity int, authorization string, coupon string, operatorId string, rotationId string) (map[string]interface{}, error) {
	return orderMake(prepareMobile(countryId, periodId, quantity, authorization, coupon, operatorId, rotationId))
}

// OrderMakeResident Create an order Resident
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param integer tarifId
// @param string coupon
// @return array Example
// [
//     'orderId' => 1000000,
//     'total' => 35.1,
//     'balance' => 10.19
// ]
func OrderMakeResident(tarifId int, coupon string) (map[string]interface{}, error) {
	return orderMake(prepareResident(tarifId, coupon))
}

func prepareIpv4(countryId int, periodId string, quantity int, authorization string, coupon string, customTargetName string) map[string]interface{} {
	data := map[string]interface{}{
		"paymentId":        paymentId,
		"generateAuth":     generateAuth,
		"countryId":        countryId,
		"periodId":         periodId,
		"quantity":         quantity,
		"authorization":    authorization,
		"coupon":           coupon,
		"customTargetName": customTargetName,
	}
	return data
}

func prepareIpv6(countryId int, periodId string, quantity int, authorization string, coupon string, customTargetName string, protocol string) map[string]interface{} {
	data := map[string]interface{}{
		"paymentId":        paymentId,
		"generateAuth":     generateAuth,
		"countryId":        countryId,
		"periodId":         periodId,
		"quantity":         quantity,
		"authorization":    authorization,
		"coupon":           coupon,
		"customTargetName": customTargetName,
		"protocol":         protocol,
	}
	return data
}

func prepareMobile(countryId int, periodId string, quantity int, authorization string, coupon string, operatorId string, rotationId string) map[string]interface{} {
	data := map[string]interface{}{
		"paymentId":     paymentId,
		"generateAuth":  generateAuth,
		"countryId":     countryId,
		"periodId":      periodId,
		"quantity":      quantity,
		"authorization": authorization,
		"coupon":        coupon,
		"operatorId":    operatorId,
		"rotationId":    rotationId,
	}
	return data
}

func prepareResident(tarifId int, coupon string) map[string]interface{} {
	data := map[string]interface{}{
		"paymentId": paymentId,
		"tarifId":   tarifId,
		"coupon":    coupon,
	}
	return data
}

// Calculate the order
func orderCalc(data map[string]interface{}) (map[string]interface{}, error) {
	result, err := Request("POST", "order/calc", data)
	return result.Map, err
}

// Create an order
func orderMake(data map[string]interface{}) (map[string]interface{}, error) {
	result, err := Request("POST", "order/make", data)
	return result.Map, err
}

/////////////////////////////// Proxy ///////////////////////////////

// ProxyList Proxies list
// @param string proxyType - ipv4 | ipv6 | mobile | isp | mix | ""
// @return array Example
// [
//     'id' => 9876543,
//     'order_id' => 123456,
//     'basket_id' => 9123456,
//     'ip' => 127.0.0.2,
//     'ip_only' => 127.0.0.2,
//     'protocol' => 'HTTP',
//     'port_socks' => 50101,
//     'port_http' => 50100,
//     'login' => 'login',
//     'password' => 'password',
//     'auth_ip' => '',
//     'rotation' => '',
//     'link_reboot' => '#',
//     'country' => 'France',
//     'country_alpha3' => 'FRA',
//     'status' => 'Active',
//     'status_type' => 'ACTIVE',
//     'can_prolong' => 1,
//     'date_start' => '26.06.2023',
//     'date_end' => '26.07.2023',
//     'comment' => '',
//     'auto_renew' => 'Y',
//     'auto_renew_period' => ''
// ]
func ProxyList(proxyType string) (map[string]interface{}, error) {
	result, err := Request("GET", "proxy/list/"+proxyType, nil)
	return result.Map, err
}

// ProxyDownload Proxy export of certain type in txt or csv
// @param string type - ipv4 | ipv6 | mobile | isp | mix | resident
// $param string ext - txt | csv
// $param string proto - https | socks5 | ''
// $param integer listId - only for resident, if not set - will return ip from all sheets
// @return string Example
// login:password@127.0.0.2:50100
func ProxyDownload(proxyType string, ext string, proto string, listId string) string {
	return string(RequestBinary("proxy/download/" + proxyType + "?ext=" + ext + "&proto=" + proto + "&listId=" + listId))
}

// ProxyCommentSet Set proxy comment
// @param array ids Any id, regardless of the type of proxy
// @param string comment
// @return integer Count updated proxy
func ProxyCommentSet(ids string, comment string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"ids":     ids,
		"comment": comment,
	}
	result, err := Request("POST", "proxy/comment/set", data)
	return result.Map, err
}

/////////////////////////////// Tools ///////////////////////////////

// ProxyCheck Check single proxy
// @param string $proxy Available values - user:password@127.0.0.1:8080, user@127.0.0.1:8080, 127.0.0.1:8080
// @return array Example result:
//  [
//      'ip' => '127.0.0.1',
//      'port' => 8080,
//      'user' => 'user',
//      'password' => 'password',
//      'valid' => true,
//      'protocol' => 'HTTP',
//      'time' => 1234
//  ]
func ProxyCheck(proxy string) (map[string]interface{}, error) {
	result, err := Request("GET", "tools/proxy/check?proxy="+proxy, nil)
	return result.Map, err
}

/////////////////////////////// System ///////////////////////////////

// Ping Check service availability
// @return timestamp
func Ping() float64 {
	result, err := Request("GET", "system/ping", nil)
	if err != nil {
		return -1
	}
	if result.Map["pong"] != "" {
		return result.Map["pong"].(float64)
	}
	return -1
}

/////////////////////////////// Resident ///////////////////////////////

// ResidentPackage Package Information
// Remaining traffic, end date
// @return array Example
// [
//     'is_active': true,
//     'rotation': 60,
//     'tarif_id': 2,
//     'traffic_limit': 7516192768,
//     'traffic_usage': 10,
//     'expired_at': "d.m.Y H:i:s",
//     'auto_renew': false
// ]
//
func ResidentPackage() (map[string]interface{}, error) {
	result, err := Request("GET", "resident/package", nil)
	return result.Map, err
}

// ResidentGeo Database geo locations (zip ~300Kb, unzip ~3Mb)
// @return binary
func ResidentGeo() []byte {
	return RequestBinary("resident/geo")
}

// ResidentList List of existing ip list in a package
// You can download the list via endpoint /proxy/download/resident?listId=123
// @return array
func ResidentList() ([]interface{}, error) {
	result, err := Request("GET", "resident/lists", nil)
	return result.Slice, err
}

// ResidentListAdd Create list in package
// You can download the list via endpoint /proxy/download/resident?listId=123
// @return array
func ResidentListAdd(title string, whitelist string, country string, region string, city string, isp string) (map[string]interface{}, error) {
	geo := map[string]interface{}{
		"country": country,
		"region":  region,
		"city":    city,
		"isp":     isp,
	}
	data := map[string]interface{}{
		"title":     title,
		"whitelist": whitelist,
		"geo":       geo,
	}
	result, err := Request("POST", "resident/list", data)
	return result.Map, err
}

// ResidentListRename Rename list in user package
// @param integer $id - listId
// @param string $title
// @return array Updated list model
func ResidentListRename(id string, title string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"id":    id,
		"title": title,
	}
	result, err := Request("POST", "resident/list/rename", data)
	return result.Map, err
}

// ResidentListDelete Remove list from user package
// @param integer $id - listId
// @return array Updated list model
func ResidentListDelete(id string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"id": id,
	}
	result, err := Request("DELETE", "resident/list/delete", data)
	return result.Map, err
}

//////////////////////////// Resident Subusers ////////////////////////////

// ResidentsubuserPackages Package Information
// Remaining traffic, expiration date
//
// @return array Example
// [[
//     'is_active': true,
//     'rotation': 60,
//     'traffic_limit': 7516192768,
//     'traffic_usage': 10,
//     'expired_at': "d.m.Y H:i:s"
// ]]
//
func ResidentsubuserPackages() ([]interface{}, error) {
	result, err := Request("GET", "residentsubuser/packages", nil)
	return result.Slice, err
}

// ResidentsubuserCreate Create subpackage
// Creates a subpackage from your current active tariff with traffic.
// The validity period must not exceed the current active tariff validity period with traffic
// The amount of traffic is specified in bytes.
// The specified amount of traffic will be reserved from the current active tariff with traffic, but not more than is available in the active tariff with traffic.
// Rotation can take values:
// -1 No rotation
// 0 Every request
// 1...3600 interval in seconds
//
// @param integer rotation -1...3600
// @param integer traffic_limit - in bytes
// @param string expired_at
// @return array Example
// [
//     'is_active': true,
//     'rotation': 60,
//     'traffic_limit': 7516192768,
//     'traffic_usage': 10,
//     'expired_at': "d.m.Y H:i:s"
// ]
//
func ResidentsubuserCreate(rotation int, traffic_limit int, expired_at string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"rotation":      rotation,
		"traffic_limit": traffic_limit,
		"expired_at":    expired_at,
	}
	result, err := Request("POST", "residentsubuser/create", data)
	return result.Map, err
}

// ResidentsubuserUpdate Update subpackage
// When deactivating a subpackage, the remaining traffic will stay in the subpackage.
// The validity period must not exceed the current active tariff validity period with traffic
// The total amount of traffic in bytes that should be in the subpackage is specified
// Rotation can take values:
// -1 No rotation
// 0 Every request
// 1...3600 interval in seconds
//
// @param integer rotation -1...3600
// @param integer traffic_limit - in bytes
// @param string expired_at
// @param string is_active
// @param string package_key
// @return array Example
// [
//     'is_active': true,
//     'rotation': 60,
//     'traffic_limit': 7516192768,
//     'traffic_usage': 10,
//     'expired_at': "d.m.Y H:i:s"
// ]
//
func ResidentsubuserUpdate(rotation int, traffic_limit int, expired_at string, is_active bool, package_key string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"rotation":      rotation,
		"traffic_limit": traffic_limit,
		"expired_at":    expired_at,
		"is_active":     is_active,
		"package_key":   package_key,
	}
	result, err := Request("POST", "residentsubuser/update", data)
	return result.Map, err
}

// ResidentsubuserDelete Delete subpackage
// When deleting, the unused remaining traffic of the subpackage will be returned
//
// @param string package_key
// @return array Example
// [
//     'is_active': true,
//     'rotation': 60,
//     'traffic_limit': 7516192768,
//     'traffic_usage': 10,
//     'expired_at': "d.m.Y H:i:s"
// ]
//
func ResidentsubuserDelete(package_key string) (map[string]interface{}, error) {
	result, err := Request("DELETE", "residentsubuser/delete?package_key="+package_key, nil)
	return result.Map, err
}

// ResidentsubuserLists List of existing IP lists in the subpackage
// The list can be downloaded via the endpoint /proxy/download/resident?listId=123&package_key=123456789
// @param string package_key
// @return array
func ResidentsubuserLists(package_key string) ([]interface{}, error) {
	result, err := Request("GET", "residentsubuser/lists?package_key="+package_key, nil)
	return result.Slice, err
}

// ResidentsubuserListAdd Create list in subpackage
// You can download the list via endpoint /proxy/download/resident?listId=123&package_key=123456789
// @return array
func ResidentsubuserListAdd(package_key string, title string, whitelist string, country string, region string, city string, isp string) (map[string]interface{}, error) {
	geo := map[string]interface{}{
		"country": country,
		"region":  region,
		"city":    city,
		"isp":     isp,
	}
	data := map[string]interface{}{
		"package_key": package_key,
		"title":       title,
		"whitelist":   whitelist,
		"geo":         geo,
	}
	result, err := Request("POST", "residentsubuser/list/add", data)
	return result.Map, err
}

// ResidentsubuserListRename Rename list in subuser package
// @param integer id - listId
// @param string title
// @param string package_key
// @return array Updated list model
func ResidentsubuserListRename(package_key string, id int, title string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"package_key": package_key,
		"id":          id,
		"title":       title,
	}
	result, err := Request("POST", "residentsubuser/list/rename", data)
	return result.Map, err
}

// ResidentsubuserListDelete Remove list from subuser package
// @param integer id - listId
// @return array Updated list model
func ResidentsubuserListDelete(package_key string, id int) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"package_key": package_key,
		"id":          id,
	}
	result, err := Request("DELETE", "residentsubuser/list/delete", data)
	return result.Map, err
}

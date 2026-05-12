# proxy-seller golang api

Install from [github.com](https://github.com/proxy-seller/userApiGolang)
```sh
go get github.com/proxy-seller/userApiGolang
```

## Quick start
Get API key [here](https://proxy-seller.com/personal/api/)
```go
import (
	"fmt"
	api "github.com/proxy-seller/userApiGolang"
)
func main() {
	api.SetApiKey("YOUR_API_KEY")
	api.SetPaymentId(1) // used in all calc/make requests (payment id=1(inner balance), id=43(subscribed card))
	api.SetGenerateAuth("N") // used in all calc/make requests (Y/N, default N)
	fmt.Println(api.Balance())
}
```

## Changelog
```
10.06.2024

New methods:
+ ResidentListAdd
+ ResidentsubuserPackages
+ ResidentsubuserCreate
+ ResidentsubuserUpdate
+ ResidentsubuserDelete
+ ResidentsubuserLists
+ ResidentsubuserListAdd
+ ResidentsubuserListRename
+ ResidentsubuserListDelete
```

## Methods available:
* AuthList
* AuthActive
* Balance
* BalanceAdd
* BalancePaymentsList
* ReferenceList
* OrderCalcIpv4
* OrderCalcIsp
* OrderCalcMix
* OrderCalcIpv6
* OrderCalcMobile
* OrderCalcResident
* OrderMakeIpv4
* OrderMakeIsp
* OrderMakeMix
* OrderMakeIpv6
* OrderMakeMobile
* OrderMakeResident
* ProlongCalc
* ProlongMake
* ProxyList
* ProxyDownload
* ProxyCommentSet
* ProxyCheck
* Ping
* ResidentPackage
* ResidentGeo
* ResidentList
* ResidentListAdd
* ResidentListRename
* ResidentListDelete
* ResidentsubuserPackages
* ResidentsubuserCreate
* ResidentsubuserUpdate
* ResidentsubuserDelete
* ResidentsubuserLists
* ResidentsubuserListAdd
* ResidentsubuserListRename
* ResidentsubuserListDelete
# evcc-Fork: Abweichungen von upstream/master

Stand: Branch `testing` vs. `upstream/master`. Fasst den **Netto-Diff** zusammen
– diese Datei beschreibt den tatsächlich aktiven Endstand, nicht die
Commit-Historie.

⚠️ **Build-Kopplung:** `go.mod` enthält `replace github.com/evcc-io/optimizer =>
../optimizer`, um gegen den lokalen Optimizer-Fork mit `withhold_charge` zu
bauen (evcc-io/optimizer#79). Muss vor jedem Upstream-Merge/PR entfernt werden,
sobald ein offizielles Optimizer-Release das Feature enthält.

Backup-Branches:
- `archive/hems-external-control-lease` — Stand vor Entfernung von Paket A
  (External Control Lease + Octopus/Tibber HEMS)
- `archive/battery-charge-power-push-stale-merge` — der versehentlich gemergte
  **veraltete** Stand von `feat/battery-charge-power-push` (stale lokaler
  Branch-Ref, drei Einzelcommits mit separaten Watchdogs statt der finalen
  Memory-Cell-Lösung). Nur zur Nachvollziehbarkeit, nicht mehr relevant.

---

## Paket A — Optimizer-gesteuertes Battery HoldCharge / Einspeisespitzen-Kappung

Lässt den Optimizer den Batteriemodus (`normal`/`hold`/`charge`/`holdcharge`)
pro Slot vorschlagen und setzt diesen Vorschlag automatisch um, um eine
Einspeisegrenze (z.B. Solarspitzengesetz) einzuhalten, statt PV-Überschuss
abzuregeln.

**Core-Logik:**
- `core/site_battery.go`: `holdChargeMode()` (kollabiert Multi-Batterie-Vorschläge
  auf einen globalen Modus, Priorität holdcharge > hold > charge > normal),
  `holdChargePlanAvailable()` (Plan max. 30 Min alt), Einbindung in
  `requiredBatteryMode()` (nur wenn `!batteryEstimator`, siehe Paket C –
  schließt sich mit dem Estimator gegenseitig aus)
- `core/site_optimizer.go`: `currentSlotSuggestion()`, `gridExportLimit`,
  `peakExportSlot`/`holdChargeGate`, `holdChargeSuggestions` pro Batterie
- `core/site.go` / `core/site_api.go`: Settings `BatteryAutoHoldCharge`,
  `BatteryHoldChargeAlways`, `GridExportLimit`

**Push-Mechanismus (Memory-Cell-Design, finaler Stand von
`feat/battery-charge-power-push` @ `1609c08eb`):**
Der geplante Lade-/Cap-Wert wird nicht mehr per generischem `plugin/state.go`
(jq-Pull aus einem globalen Cache) ins Kostal-Template gezogen, sondern über
typisierte Capability-Interfaces direkt vom Core gepusht – konsistent mit
`SetBatteryMode`/`SetCurtailPercent`:
- `api/api.go`: `BatteryChargePowerLimiter.SetMaxChargePower(watt)`,
  `BatteryChargeSetpointController.SetChargeSetpoint(watt)`
- `plugin/memory.go` (+Test): eine **gerätescoped, namensadressierte
  Werte-Zelle**. Als Sink speichert ihr Setter den gepushten Wert; als Forward
  (`set:`) liest sie den gespeicherten Wert und schreibt ihn weiter (Input
  ignoriert). Scoping rein über Go-`context.Context`
  (`plugin.WithMemoryStore`, ein Store pro Meter-Konstruktion in `meter.go`) –
  **nicht** namensbasiert, kein Bezug zu `withDeviceName`/`.name`.
- `core/site_battery.go`: `updateBatteryChargeValues()` pusht beide Werte
  (`SetMaxChargePower`, `SetChargeSetpoint`) bei jedem Update-Zyklus in die
  Zellen des jeweiligen Geräts, unconditional (Lifecycle lebt am
  `batterymode`-Case, der sie liest, nicht hier).
- `templates/definition/meter/kostal-plenticore-gen2.yaml`: `batterymode`
  case 3 (`charge`) und case 4 (`holdcharge`) lesen die Zellen
  (`source: memory, name: chargesetpoint` / `maxchargepowerlimit`) und
  schreiben Register 1034/1038 **innerhalb des bestehenden `batterymode`-
  Watchdogs** (ein Watchdog, `defer: true`, `reset: 1`).

**Warum nicht zwei separate Watchdogs (verworfener Zwischenstand):** Eine
eigene `watchdog`-Plugin-Instanz pro Wert (`maxchargepowerlimit`/
`chargesetpoint` als eigenständige Top-Level-Keys) kennt den Batteriemodus
nicht und hat keinen sinnvollen `reset:`-Sentinel für einen kontinuierlichen
Leistungswert – sie hätte entweder ewig weitergeschrieben (auch nach Verlassen
von holdcharge/charge) oder einen invertierten Reset-Wert gebraucht. Die
Memory-Zelle bindet die Register-Schreibvorgänge stattdessen an den
`batterymode`-Switch, der bereits `reset: 1` (stoppt bei Rückkehr zu normal)
und `defer: true` (Timing-Schutz) hat – ohne zweiten Watchdog oder
Schatten-Zustand. Dieser Zwischenstand (separate Watchdogs, drei Einzelcommits
endend auf `e6f13ae52`) wurde versehentlich gemergt (stale lokaler Branch-Ref,
kein `git fetch` vor dem Merge) und später durch den echten finalen Commit
`1609c08eb` von `origin/feat/battery-charge-power-push` ersetzt. Siehe
`archive/battery-charge-power-push-stale-merge`.

**Bekannte, behobene Fallback-Lücke:** `updateBatteryChargeValues()` fällt für
`SetChargeSetpoint` auf `BatteryPowerLimiter.GetPowerLimits()` zurück, wenn
kein aktueller Optimizer-Plan für die Batterie vorliegt (z.B. Optimizer nicht
konfiguriert, Plan stale). Das Kostal-Template hatte diese generische
Capability nie verdrahtet, wodurch ein rein tarifbasiert (`BatteryGridChargeLimit`,
unabhängig vom Optimizer) ausgelöstes Grid-Charge dauerhaft bei 0 W geblieben
wäre. Fix: `maxChargePower`/`maxDischargePower` (aus den vorhandenen,
optionalen `advanced`-Params `maxchargepower`/`maxdischargepower` ohne
Default) werden jetzt gerendert und über `meter.go`s generisches
`batteryPowerLimits`-Squash zu `BatteryPowerLimiter` dekoriert – kein
Custom-Code, nur bei jeweils gesetztem Wert aktiv.

**UI:**
- `assets/js/components/Battery/BatteryConfigCard.vue`,
  `BatteryUsageSettings.vue`, `BatteryExperimental.vue`: Schalter "HoldCharge"
  (🧪 experimentell) + "auch ohne erkannte Spitze"
- `assets/js/components/Config/ControlModal.vue`: Feld "Einspeisegrenze" (W)

**Offene Frage:** Ob `BatteryHoldChargeAlways` ("Stufe 2") noch gebraucht
wird, hängt davon ab, ob die weiche Feed-in-Glättung im Optimizer (siehe
optimizer-Repo, Paket 1) in der Praxis spürbar ist. Ohne gesetzte
`gridExportLimit` ist auch Stufe 1 wirkungslos.

**Gegenstück im optimizer-Repo:** siehe optimizer-Paket 1 (`withhold_charge`,
Feed-in-Shaping).

---

## Paket B — SoC-Depletion-Kosten (Batteriealterung / Reserve-Komfort)

- `core/site_optimizer.go`: Konstanten `socDepletionCostHigh` (0.005 €/h, ab
  80% SOC), `socDepletionCostLow` (0.002 €/h, unter 20% SOC), gesetzt via
  `bat.PrcDplSocHigh`/`PrcDplSocLow` in `batteryRequest()`

**Gegenstück im optimizer-Repo:** optimizer-Paket 2 (`prc_dpl_soc_high/low`).

---

## Paket C — Battery Estimator (PV-Spread-Charging, unabhängig vom Optimizer)

Alternative zu Paket A für Setups ohne (aktiven) Optimizer: verteilt die
PV-Ladeleistung gleichmäßig über den Tag, sodass die Batterie erst zu einer
Zielzeit (Default 18:00) voll ist.

- `core/site_battery_estimator.go` + Test, `core/site_holdcharge_test.go`
- `core/site.go`/`site_api.go`: Settings `BatteryEstimator`,
  `BatteryEstimatorFactor` (Default 1.5), `BatteryEstimatorTargetTime`
- `core/site_battery.go`: schließt sich mit `batteryAutoHoldCharge`
  (Paket A) gegenseitig aus
- UI: `BatteryConfigCard.vue` – Schalter "Estimator" 🧪

---

## Paket D — Robustere Solar-/Verbrauchsprognose (Median-Skalierung, Reserve-Margin)

- `core/site_tariffs.go`: `solarScaleMedian()` (28-Tage-Median),
  `consumptionMargin()` (80. Perzentil-Reserve), beide nur aktiv mit
  `OptimizerForecastAdjust`
- UI: `assets/js/views/Optimize.vue`, `assets/js/components/Forecast/types.ts`

---

## Entfernt: HEMS External Control Lease (Octopus SmartCharge / Tibber GridReward)

Genereischer Lease-Mechanismus (`SetExternalControl`/`GetExternalControl` auf
Loadpoint-Ebene) plus zwei HEMS-Integrationen. In der Praxis überflüssig,
komplett aus `testing` entfernt. Vollständiger Stand vor der Entfernung auf
`archive/hems-external-control-lease`.

## Entfernt: plugin/state.go (Pull-basierte State-Lesung)

War Infrastruktur für einen früheren Zwischenstand des Peak-Shaving-Pushs
(jq-Pull aus einem globalen `ParamCache`). Seit dem Wechsel auf typisierte
Push-Capabilities (erst separate Watchdogs, dann die finale Memory-Cell-
Lösung) von keinem Template mehr genutzt. `util/param.go`s
`DefaultParamCache`-Singleton und `cmd/setup.go`s `withDeviceName`
(Template-Selbstreferenz per `.name`) waren nur für dieses Plugin da und
sind ebenfalls entfernt – alle drei Dateien wieder identisch mit
`upstream/master`.

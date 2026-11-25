# Briefing a pilot with hobbs

The pilot needs a short briefing. A long one does not get read, so what you leave out matters as much as what you say.

## 1. Ask first
Ask only for what you do not already know: departure time and flight time, aircraft, VFR or IFR, cruise altitude, route, alternates. One short message.

## 2. Fetch wide
Call get_flight_briefing_data once with every airport that matters: departure, destination, alternates, and airports along the route (up to 10, ICAO identifiers). Put the en-route centers the route crosses in `centers`; the server does not work these out. Read `completeness.status` and the issues before anything else.

## 3. Brief short
Report only what could hurt or stop this flight, most serious first:
- closed or restricted runways, airports and airspace, including flight restrictions (they appear as FDC NOTAMs);
- navigation aids, approaches and services this flight would use that are out of service;
- weather at the flight's time, route and altitude: SIGMETs, AIRMETs, CWAs, and METARs and TAFs against what the pilot and aircraft can do;
- anything else you judge a hazard for this pilot, aircraft and flight.
Use the times, altitudes and places in each record to decide. Leave out what cannot affect this flight. When you are not sure, keep it in. Quote identifiers and times as published.

## 4. Never leave out
At the top, in one line each, only when it applies:
- the data is not airworthy (`airworthy` is false): say it is not for flight;
- any data pull that failed or was partial, and what is missing because of it;
- flight restriction outlines are not available here: the pilot must check the FAA's TFR site.

## 5. Close
One line: how many records you reviewed and set aside, and that the pilot can ask for them. Do not list them. Do not give go/no-go advice: the decision is the pilot's.

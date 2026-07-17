#!/usr/bin/env python3
"""
Disambiguation benchmark for the concept-extraction context list.

Tests whether a reduced context list (e.g. the 87-category manual_select.json)
preserves the disambiguation power of the original 789-label DBpedia L3 list.

Method:
  1. Craft ambiguous facts (same surface concept, different real-world meaning).
  2. Send each fact to the concept-extraction model (google/gemma-4-31b-it via
     OpenRouter) TWICE: once with the original 789-label prompt, once with the
     candidate list (e.g. the 87 categories).
  3. Compare the (concept, context) pairs the model returns.
  4. Score: does each list assign the RIGHT disambiguating context? Does the
     candidate list still separate the two meanings, or does it collapse them?

Outputs (next to the script):
  disambiguation_benchmark.json   full results
  stdout                          summary table

Usage:
  set -a; . .env; python3 scripts/experiments/disambiguation_benchmark.py
  # or with a specific candidate list:
  python3 scripts/experiments/disambiguation_benchmark.py \
      --candidate scripts/experiments/manual_select.json
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.request
import urllib.error
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent
DEFAULT_ORIGINAL = (
    Path(__file__).resolve().parents[2]
    / "backend/internal/providers/ontology/dbpedia_l3.json"
)
DEFAULT_CANDIDATE = SCRIPT_DIR / "manual_select.json"
DEFAULT_OUT = SCRIPT_DIR / "disambiguation_benchmark.json"

OPENROUTER_URL = "https://openrouter.ai/api/v1/chat/completions"
CHAT_MODEL = "google/gemma-4-31b-it"

PROMPT_TEMPLATE = """You are a concept extraction system. Given a single atomic fact below, extract ALL relevant concepts it mentions.

## What is a concept

A concept is a named entity or idea the fact refers to. Extract:
- People (full names when available): "Donald Trump", "Albert Einstein"
- Places: "Paris", "Silicon Valley"
- Molecules / chemical compounds: "DNA", "graphene oxide"
- Organizations: "MIT", "Electronic Frontier Foundation"
- Ideas, theories, methods: "general relativity", "Sanger sequencing"
- Standalone names the fact is about

Each concept must be one or two words. Full names and organization names may be longer. Prefer the most specific named form present in the fact.

## Context assignment

Every concept must be assigned a context drawn EXACTLY from the L3 ontology class list below. The context is the class that best describes what kind of thing the concept is (a person, a chemical compound, an organization, a work, etc.). Pick the single best-fitting label from the list — do not invent labels outside the list. When a label carries a short description (shown after the em-dash), use it as a hint to pick the right label.

## Seed aliases

For each concept, also emit 2-3 seed aliases: short forms, initials, acronyms, or common alternate names that refer to the same thing. Examples:
- "Donald Trump" -> ["Donald J. Trump", "DJT", "Trump"]
- "DNA" -> ["deoxyribonucleic acid", "deoxyribonucleic"]
- "MIT" -> ["Massachusetts Institute of Technology", "Massachusetts Institute of Tech"]

Seed aliases boost the chance that the next fact mentioning the same concept under a different surface form matches an existing concept via text search, so be generous but accurate.

## L3 ontology class list (the context MUST come from this list)

%s

## Rules
- Extract EVERY relevant concept the fact mentions, not just the primary subject.
- Concepts are 1-2 words (full names / org names may be longer).
- The context MUST be one of the labels in the list above, verbatim.
- 2-3 seed aliases per concept.
- Skip concepts that are not named explicitly in the fact (no inference beyond the text).
- If the fact mentions no extractable concepts, return [].

## Fact text
\"\"\"
%s
\"\"\"

Respond with a JSON array of objects, like:
[{"concept":"Donald Trump","context":"Politician","seed_aliases":["Donald J. Trump","DJT"]},{"concept":"DNA","context":"Molecule","seed_aliases":["deoxyribonucleic acid","deoxyribonucleic"]}]
Respond with ONLY the JSON array, no other text."""

# The benchmark cases: each has a fact, the target concept to check,
# the expected context (ground truth), and a note on WHY it's ambiguous.
BENCHMARK_CASES = [
    {
        "id": "cancer_disease_vs_journal",
        "fact": "A 2023 study published in Cancer found that early-stage melanoma patients had a 94% survival rate when treated with combination immunotherapy.",
        "target_concept": "Cancer",
        "expected_context_original": "disease",
        "expected_context_candidate": "Disease",
        "ambiguity": "Cancer is both a disease AND a journal name; the fact mentions both meanings (the disease and the journal it was published in).",
        "disambiguation_test": "The model should assign context 'disease' (or 'Disease') to the concept Cancer because the fact is about melanoma survival, not about the journal itself.",
    },
    {
        "id": "coffee_beverage_vs_food_vs_book",
        "fact": "Coffee consumption of more than 4 cups per day is associated with reduced risk of liver cirrhosis according to a meta-analysis.",
        "target_concept": "Coffee",
        "expected_context_original": "beverage",
        "expected_context_candidate": "Beverage",
        "ambiguity": "Coffee can be a beverage, a food, or a book about coffee. The fact is about drinking coffee.",
        "disambiguation_test": "Should get 'beverage' or 'Beverage' — the fact is about consumption.",
    },
    {
        "id": "water_compound_vs_body_of_water",
        "fact": "The water in the Pacific Ocean covers approximately 165 million square kilometers, making it the largest geographic feature on Earth.",
        "target_concept": "Water",
        "expected_context_original": "body of water",
        "expected_context_candidate": "Geographic feature",
        "ambiguity": "Water is both a chemical compound (H2O) and a body of water (ocean, lake). The fact refers to the Pacific Ocean as a body of water.",
        "disambiguation_test": "Should get 'body of water' or 'Geographic feature' — the fact refers to a geographic body of water.",
    },
    {
        "id": "lourdes_place_vs_novel",
        "fact": "Lourdes is a commune in the Hautes-Pyrenees department of France, situated at the foot of the Pyrenees mountain range.",
        "target_concept": "Lourdes",
        "expected_context_original": "place",
        "expected_context_candidate": "Place",
        "ambiguity": "Lourdes is both a town in France and a novel by Emile Zola. The fact describes the geographic town.",
        "disambiguation_test": "Should get 'place' or 'Place' or 'city' or 'Settlement' — the fact describes geography.",
    },
    {
        "id": "universe_celestial_vs_concept",
        "fact": "The universe began 13.8 billion years ago with the Big Bang, expanding from an extremely hot, dense initial state.",
        "target_concept": "Universe",
        "expected_context_original": "celestial body",
        "expected_context_candidate": "Space",
        "ambiguity": "Universe could be a celestial body, a place, or a Scientific concept. The fact is about cosmology.",
        "disambiguation_test": "Should get 'celestial body' or 'Space' — it's about the cosmos.",
    },
    {
        "id": "energy_concept_vs_quantity",
        "fact": "The energy released by the Hiroshima bomb was approximately 63 terajoules, equivalent to 15 kilotons of TNT.",
        "target_concept": "Energy",
        "expected_context_original": "Scientific concept",
        "expected_context_candidate": "Concept",
        "ambiguity": "Energy is a Scientific concept but also a measurable quantity. The fact uses it as a physics concept.",
        "disambiguation_test": "Should get 'Scientific concept' or 'Concept'.",
    },
    {
        "id": "soil_mineral_vs_concept",
        "fact": "The soil in the Amazon basin contains high levels of aluminum and iron oxides, making it acidic and low in nutrients.",
        "target_concept": "Soil",
        "expected_context_original": "topical concept",
        "expected_context_candidate": "Concept",
        "ambiguity": "Soil could be a mineral, a chemical substance, or a topical concept. The fact describes soil composition.",
        "disambiguation_test": "Should get 'topical concept' or 'Concept' or 'mineral' — soil as a topic.",
    },
    {
        "id": "pain_disease_vs_concept",
        "fact": "Chronic pain affects over 1.5 billion people worldwide and is defined as pain lasting longer than 3 months.",
        "target_concept": "Pain",
        "expected_context_original": "disease",
        "expected_context_candidate": "Disease",
        "ambiguity": "Pain is both a disease (chronic pain syndrome) and a Scientific concept (nociception).",
        "disambiguation_test": "Should get 'disease' or 'Disease' — the fact treats pain as a medical condition.",
    },
    {
        "id": "placebo_medicine_vs_concept",
        "fact": "The placebo effect was demonstrated in a clinical trial where 32% of patients receiving sugar pills reported pain reduction.",
        "target_concept": "Placebo",
        "expected_context_original": "Medicine",
        "expected_context_candidate": "Disease",
        "ambiguity": "Placebo is both a Medicine and a Scientific concept. The fact is about the medical/clinical use.",
        "disambiguation_test": "Should get 'Medicine' or 'Disease' — the clinical context.",
    },
    {
        "id": "drought_event_vs_concept",
        "fact": "The drought that affected the Sahel region from 1968 to 1985 caused an estimated 100,000 deaths and displaced millions.",
        "target_concept": "Drought",
        "expected_context_original": "natural event",
        "expected_context_candidate": "Event",
        "ambiguity": "Drought is both a natural event and a Scientific concept. The fact describes a specific historical event.",
        "disambiguation_test": "Should get 'natural event' or 'Event' — it's about an event.",
    },
    {
        "id": "california_state_vs_region",
        "fact": "California became the 31st state of the United States on September 9, 1850, after being ceded by Mexico.",
        "target_concept": "California",
        "expected_context_original": "state",
        "expected_context_candidate": "Administrative region",
        "ambiguity": "California is both a US state (administrative region) and a geographic region. The fact is about statehood.",
        "disambiguation_test": "Should get 'state' or 'Administrative region' or 'Standard' — political entity.",
    },
    {
        "id": "nature_journal_vs_concept",
        "fact": "The journal Nature, founded in 1869 by Norman Lockyer, is one of the oldest and most cited scientific publications in the world.",
        "target_concept": "Nature",
        "expected_context_original": "academic journal",
        "expected_context_candidate": "Media publication",
        "ambiguity": "Nature is both an academic journal and a topical concept (the natural world). The fact is about the journal.",
        "disambiguation_test": "Should get 'academic journal' or 'Media publication' — it's about the publication.",
    },
    {
        "id": "general_relativity_concept_vs_book",
        "fact": "Einstein's general relativity predicts that light bends around massive objects, confirmed by Eddington's 1919 eclipse expedition.",
        "target_concept": "General Relativity",
        "expected_context_original": "Scientific concept",
        "expected_context_candidate": "Concept",
        "ambiguity": "General relativity is both a Scientific concept and a book title. The fact describes the theory.",
        "disambiguation_test": "Should get 'Scientific concept' or 'Concept'.",
    },
    {
        "id": "reiki_activity_vs_concept",
        "fact": "Reiki practitioners place their hands lightly on or near the patient's body to channel healing energy during sessions lasting 45-90 minutes.",
        "target_concept": "Reiki",
        "expected_context_original": "activity",
        "expected_context_candidate": "Activity",
        "ambiguity": "Reiki is both an activity (the practice) and a Scientific concept (the energy theory).",
        "disambiguation_test": "Should get 'activity' or 'Activity' — the fact describes the practice.",
    },
    {
        "id": "climate_change_event_vs_concept",
        "fact": "Climate change has caused global average temperatures to rise by 1.1 degrees Celsius since the pre-industrial era.",
        "target_concept": "Climate Change",
        "expected_context_original": "Scientific concept",
        "expected_context_candidate": "Concept",
        "ambiguity": "Climate change is both a Scientific concept and a natural event. The fact describes the phenomenon.",
        "disambiguation_test": "Should get 'Scientific concept' or 'Concept' or 'natural event' or 'Event'.",
    },
    {
        "id": "clinical_trial_concept_vs_project",
        "fact": "A phase III clinical trial with 2,847 participants demonstrated that the new drug reduced mortality by 23% compared to placebo.",
        "target_concept": "Clinical Trial",
        "expected_context_original": "Scientific concept",
        "expected_context_candidate": "Concept",
        "ambiguity": "Clinical trial is both a Scientific concept and a research project. The fact describes the methodology.",
        "disambiguation_test": "Should get 'Scientific concept' or 'Concept' or 'research project' or 'Activity'.",
    },
    {
        "id": "apple_fruit_vs_company",
        "fact": "Apple announced its new M4 chip at WWDC 2024, claiming a 50% performance improvement over the M2.",
        "target_concept": "Apple",
        "expected_context_original": "organisation",
        "expected_context_candidate": "Organisation",
        "ambiguity": "Apple is both a fruit (food) and a company (organisation). The fact is about the tech company.",
        "disambiguation_test": "Should get 'organisation' or 'Organisation' — it's about the company.",
    },
    {
        "id": "amazon_company_vs_river",
        "fact": "Amazon has over 1.5 million employees worldwide and reported $574.8 billion in revenue for fiscal year 2023.",
        "target_concept": "Amazon",
        "expected_context_original": "organisation",
        "expected_context_candidate": "Organisation",
        "ambiguity": "Amazon is both a river (geographic feature) and a company (organisation). The fact is about the company.",
        "disambiguation_test": "Should get 'organisation' or 'Organisation' — it's about the company.",
    },
    {
        "id": "trump_politician_vs_brand",
        "fact": "Trump Tower on Fifth Avenue in New York City is a 58-story mixed-use building valued at approximately $400 million.",
        "target_concept": "Trump",
        "expected_context_original": "Politician",
        "expected_context_candidate": "Politician",
        "ambiguity": "Trump is both a politician and a building/brand name. The fact is about a building but named after the person.",
        "disambiguation_test": "Should get 'Politician' — Trump as a person.",
    },
    {
        "id": "turkey_country_vs_animal_vs_food",
        "fact": "Turkey is a transcontinental country spanning the Anatolian peninsula in Western Asia and the Balkan peninsula in Southeastern Europe.",
        "target_concept": "Turkey",
        "expected_context_original": "country",
        "expected_context_candidate": "Country",
        "ambiguity": "Turkey is a country, an animal (bird), and a food. The fact is about the country.",
        "disambiguation_test": "Should get 'country' or 'Country' — it's about the nation.",
    },
    # --- 21-40: Person-type disambiguation (the custom list's strength) ---
    {
        "id": "einstein_scientist_vs_person",
        "fact": "Albert Einstein published four groundbreaking papers in 1905, including the discovery of the photoelectric effect and special relativity.",
        "target_concept": "Einstein",
        "expected_context_original": "scientist",
        "expected_context_candidate": "Scientist",
        "ambiguity": "Einstein is a scientist but could also be just a person or Academic Person.",
        "disambiguation_test": "Should get 'scientist' or 'Scientist' — the fact describes scientific work.",
    },
    {
        "id": "mozart_artist_vs_person",
        "fact": "Mozart composed his first symphony at age 8 and produced over 600 works before dying at 35.",
        "target_concept": "Mozart",
        "expected_context_original": "artist",
        "expected_context_candidate": "Artist",
        "ambiguity": "Mozart is a musical artist but could be a generic person.",
        "disambiguation_test": "Should get 'artist' or 'Artist' or 'musical artist'.",
    },
    {
        "id": "obama_politician_vs_person",
        "fact": "Obama signed the Affordable Care Act into law on March 23, 2010, extending health insurance to 20 million Americans.",
        "target_concept": "Obama",
        "expected_context_original": "politician",
        "expected_context_candidate": "Politician",
        "ambiguity": "Obama is a politician but could be a generic person or office holder.",
        "disambiguation_test": "Should get 'politician' or 'Politician'.",
    },
    {
        "id": "scorsese_director_vs_person",
        "fact": "Scorsese directed Goodfellas in 1990, using a tracking shot that has become one of the most studied sequences in cinema history.",
        "target_concept": "Scorsese",
        "expected_context_original": "actor",
        "expected_context_candidate": "Film director",
        "ambiguity": "Scorsese is a film director, not an actor. The L3 list lacks a 'director' class so may misassign.",
        "disambiguation_test": "Should get 'actor' (L3's closest) or 'Film director' (custom).",
    },
    {
        "id": "shakespeare_writer_vs_person",
        "fact": "Shakespeare wrote 39 plays and 154 sonnets during his career, with Hamlet being his longest at 4,042 lines.",
        "target_concept": "Shakespeare",
        "expected_context_original": "writer",
        "expected_context_candidate": "Writer",
        "ambiguity": "Shakespeare is a writer but could be just a person.",
        "disambiguation_test": "Should get 'writer' or 'Writer'.",
    },
    {
        "id": "tesla_scientist_vs_person_vs_company",
        "fact": "Nikola Tesla patented the alternating current motor in 1888, a foundation of modern electrical power distribution.",
        "target_concept": "Tesla",
        "expected_context_original": "scientist",
        "expected_context_candidate": "Scientist",
        "ambiguity": "Tesla is a person (Nikola), a company (Tesla Inc.), and a sports team. The fact is about the person.",
        "disambiguation_test": "Should get 'scientist' or 'Scientist' — the fact describes the inventor.",
    },
    {
        "id": "curie_scientist_vs_person",
        "fact": "Marie Curie discovered radium and polonium, winning Nobel Prizes in both physics (1903) and chemistry (1911).",
        "target_concept": "Curie",
        "expected_context_original": "scientist",
        "expected_context_candidate": "Scientist",
        "ambiguity": "Curie is a scientist, could be generic person.",
        "disambiguation_test": "Should get 'scientist' or 'Scientist'.",
    },
    {
        "id": "napoleon_politician_vs_person",
        "fact": "Napoleon crowned himself Emperor of the French in 1804 and led France through the Napoleonic Wars until 1815.",
        "target_concept": "Napoleon",
        "expected_context_original": "politician",
        "expected_context_candidate": "Politician",
        "ambiguity": "Napoleon is a politician/monarch but could be a generic person or office holder.",
        "disambiguation_test": "Should get 'politician' or 'Politician' or 'monarch'.",
    },
    {
        "id": "davinci_artist_vs_scientist",
        "fact": "Leonardo da Vinci painted the Mona Lisa between 1503 and 1519, using his sfumato technique to create atmospheric depth.",
        "target_concept": "da Vinci",
        "expected_context_original": "artist",
        "expected_context_candidate": "Artist",
        "ambiguity": "Da Vinci was both an artist and a scientist/engineer. The fact is about painting.",
        "disambiguation_test": "Should get 'artist' or 'Artist' — the fact describes a painting.",
    },
    {
        "id": "darwin_scientist_vs_writer",
        "fact": "Charles Darwin observed finch beak variations on the Galapagos Islands, leading to his theory of natural selection published in 1859.",
        "target_concept": "Darwin",
        "expected_context_original": "scientist",
        "expected_context_candidate": "Scientist",
        "ambiguity": "Darwin is a scientist but also a writer (author of On the Origin of Species).",
        "disambiguation_test": "Should get 'scientist' or 'Scientist'.",
    },
    {
        "id": "gandhi_politician_vs_person",
        "fact": "Gandhi led the 240-mile Salt March in 1930, a nonviolent protest against British salt taxation that drew international attention.",
        "target_concept": "Gandhi",
        "expected_context_original": "politician",
        "expected_context_candidate": "Politician",
        "ambiguity": "Gandhi is a political figure but could be a generic person or office holder.",
        "disambiguation_test": "Should get 'politician' or 'Politician'.",
    },
    {
        "id": "picasso_artist_vs_person",
        "fact": "Pablo Picasso co-founded the Cubist movement in 1907 with Georges Braque, producing over 20,000 artworks in his lifetime.",
        "target_concept": "Picasso",
        "expected_context_original": "artist",
        "expected_context_candidate": "Artist",
        "ambiguity": "Picasso is an artist, could be a generic person.",
        "disambiguation_test": "Should get 'artist' or 'Artist'.",
    },
    {
        "id": "jordan_athlete_vs_person",
        "fact": "Michael Jordan scored a career-high 69 points against the Cavaliers on March 28, 1990, shooting 23-of-37 from the field.",
        "target_concept": "Jordan",
        "expected_context_original": "basketball player",
        "expected_context_candidate": "Athlete",
        "ambiguity": "Jordan is an athlete (basketball player) but also a country (Jordan) and a river.",
        "disambiguation_test": "Should get 'basketball player' or 'Athlete' — the fact is about the player.",
    },
    {
        "id": "woods_athlete_vs_person",
        "fact": "Tiger Woods won his 15th major championship at the 2019 Masters, completing one of the greatest comeback stories in sports.",
        "target_concept": "Woods",
        "expected_context_original": "golf player",
        "expected_context_candidate": "Athlete",
        "ambiguity": "Woods is a golf player but also a forest (woods). The fact is about the golfer.",
        "disambiguation_test": "Should get 'golf player' or 'Athlete' — the fact is about the person.",
    },
    {
        "id": "musk_businessperson_vs_politician",
        "fact": "Elon Musk founded SpaceX in 2002 with $100 million of his PayPal fortune, aiming to reduce space transportation costs.",
        "target_concept": "Musk",
        "expected_context_original": "businessperson",
        "expected_context_candidate": "Businessperson",
        "ambiguity": "Musk is a businessperson, sometimes seen as a politician or engineer.",
        "disambiguation_test": "Should get 'businessperson' or 'Businessperson'.",
    },
    {
        "id": "pope_francis_cleric_vs_politician",
        "fact": "Pope Francis became the 266th pontiff of the Catholic Church in 2013, the first Jesuit and first from the Americas.",
        "target_concept": "Pope Francis",
        "expected_context_original": "pope",
        "expected_context_candidate": "Cleric",
        "ambiguity": "Pope Francis is a cleric (pope) but could be seen as a political leader (head of Vatican state).",
        "disambiguation_test": "Should get 'pope' or 'Cleric'.",
    },
    {
        "id": "jobs_businessperson_vs_person",
        "fact": "Steve Jobs unveiled the iPhone at Macworld 2007, calling it 'a revolutionary product that changes everything.'",
        "target_concept": "Jobs",
        "expected_context_original": "businessperson",
        "expected_context_candidate": "Businessperson",
        "ambiguity": "Jobs is a businessperson but the word 'jobs' also means employment. The fact is about the person.",
        "disambiguation_test": "Should get 'businessperson' or 'Businessperson'.",
    },
    {
        "id": "judge_judge_vs_person",
        "fact": "Judge Bork was nominated to the Supreme Court by Reagan in 1987 but was rejected by the Senate 58-42.",
        "target_concept": "Judge",
        "expected_context_original": "judge",
        "expected_context_candidate": "Judge",
        "ambiguity": "Judge is both a person type (judge) and a generic title. The fact is about a specific judge.",
        "disambiguation_test": "Should get 'judge' or 'Judge'.",
    },
    {
        "id": "nasa_organisation_vs_government",
        "fact": "NASA was established on October 1, 1958, by the National Aeronautics and Space Act, replacing NACA.",
        "target_concept": "NASA",
        "expected_context_original": "organisation",
        "expected_context_candidate": "Organisation",
        "ambiguity": "NASA is both an organisation and a government agency. The fact is about its founding.",
        "disambiguation_test": "Should get 'organisation' or 'Organisation' or 'government agency' or 'Government body'.",
    },
    {
        "id": "who_organisation_vs_person",
        "fact": "The WHO declared COVID-19 a pandemic on March 11, 2020, triggering coordinated international health responses.",
        "target_concept": "WHO",
        "expected_context_original": "organisation",
        "expected_context_candidate": "Organisation",
        "ambiguity": "WHO is an organisation (World Health Organization) but could be confused with the pronoun.",
        "disambiguation_test": "Should get 'organisation' or 'Organisation'.",
    },
    # --- 41-60: Place disambiguation ---
    {
        "id": "washington_person_vs_place",
        "fact": "Washington led the Continental Army to victory over the British at Yorktown in 1781, securing American independence.",
        "target_concept": "Washington",
        "expected_context_original": "person",
        "expected_context_candidate": "Politician",
        "ambiguity": "Washington is a person (George), a US state, and a city (DC). The fact is about the person.",
        "disambiguation_test": "Should get 'person' or 'Politician' — it's about George Washington, a military/political leader.",
    },
    {
        "id": "paris_place_vs_person",
        "fact": "Paris is the capital of France, situated on the Seine River with a metropolitan population of over 12 million.",
        "target_concept": "Paris",
        "expected_context_original": "city",
        "expected_context_candidate": "Settlement",
        "ambiguity": "Paris is a city but also a figure from Greek mythology and a person name. The fact is about the city.",
        "disambiguation_test": "Should get 'city' or 'Settlement' — the fact describes the place.",
    },
    {
        "id": "cairo_place_vs_brand",
        "fact": "Cairo is the largest city in the Arab world, with a population of over 21 million in its metropolitan area.",
        "target_concept": "Cairo",
        "expected_context_original": "city",
        "expected_context_candidate": "Settlement",
        "ambiguity": "Cairo is a city, but also a typeface and a code name. The fact is about the city.",
        "disambiguation_test": "Should get 'city' or 'Settlement'.",
    },
    {
        "id": "georgia_country_vs_state",
        "fact": "Georgia is a country in the Caucasus region, bordered by Russia to the north and Turkey to the south.",
        "target_concept": "Georgia",
        "expected_context_original": "country",
        "expected_context_candidate": "Country",
        "ambiguity": "Georgia is both a country (in Caucasus) and a US state. The fact is about the country.",
        "disambiguation_test": "Should get 'country' or 'Country'.",
    },
    {
        "id": "memphis_city_vs_person",
        "fact": "Memphis, Tennessee, located on the Mississippi River, is the second-largest city in the state with a population of 633,000.",
        "target_concept": "Memphis",
        "expected_context_original": "city",
        "expected_context_candidate": "Settlement",
        "ambiguity": "Memphis is a city but also an ancient Egyptian capital and a person name. The fact is about the US city.",
        "disambiguation_test": "Should get 'city' or 'Settlement'.",
    },
    {
        "id": "boston_place_vs_organisation",
        "fact": "Boston was founded in 1630, making it one of the oldest cities in the United States, and became a center of the American Revolution.",
        "target_concept": "Boston",
        "expected_context_original": "city",
        "expected_context_candidate": "Settlement",
        "ambiguity": "Boston is a city but also a band (Boston) and a tea party event. The fact is about the city.",
        "disambiguation_test": "Should get 'city' or 'Settlement'.",
    },
    {
        "id": "sahara_place_vs_concept",
        "fact": "The Sahara is the world's largest hot desert, covering 9.2 million square kilometers across North Africa.",
        "target_concept": "Sahara",
        "expected_context_original": "Desert",
        "expected_context_candidate": "Geographic feature",
        "ambiguity": "Sahara is a desert (geographic) but also a casino and a hotel brand.",
        "disambiguation_test": "Should get 'Desert' or 'Geographic feature'.",
    },
    {
        "id": "amazon_river_vs_company",
        "fact": "The Amazon River discharges approximately 209,000 cubic meters per second, accounting for 20% of global river flow.",
        "target_concept": "Amazon",
        "expected_context_original": "river",
        "expected_context_candidate": "Geographic feature",
        "ambiguity": "Amazon is a river, a company, and a rainforest. The fact is about the river.",
        "disambiguation_test": "Should get 'river' or 'Geographic feature' — the fact describes the river.",
    },
    {
        "id": "everest_mountain_vs_person",
        "fact": "Mount Everest stands at 8,849 meters above sea level, the highest point on Earth, located on the Nepal-China border.",
        "target_concept": "Everest",
        "expected_context_original": "mountain",
        "expected_context_candidate": "Geographic feature",
        "ambiguity": "Everest is a mountain but also a person name and a brand.",
        "disambiguation_test": "Should get 'mountain' or 'Geographic feature'.",
    },
    {
        "id": "rome_place_vs_empire",
        "fact": "Rome was founded on April 21, 753 BC, according to tradition, and grew from a small settlement to the center of a vast empire.",
        "target_concept": "Rome",
        "expected_context_original": "city",
        "expected_context_candidate": "Settlement",
        "ambiguity": "Rome is a city, an empire, and a historical concept. The fact is about the city's founding.",
        "disambiguation_test": "Should get 'city' or 'Settlement'.",
    },
    {
        "id": "nile_river_vs_concept",
        "fact": "The Nile flows 6,650 kilometers from Lake Victoria to the Mediterranean, making it the longest river in Africa.",
        "target_concept": "Nile",
        "expected_context_original": "river",
        "expected_context_candidate": "Geographic feature",
        "ambiguity": "Nile is a river but also a brand name and a color.",
        "disambiguation_test": "Should get 'river' or 'Geographic feature'.",
    },
    {
        "id": "silk_road_infrastructure_vs_concept",
        "fact": "The Silk Road was a network of trade routes spanning 6,400 km, connecting China to the Mediterranean from 130 BC to 1453 AD.",
        "target_concept": "Silk Road",
        "expected_context_original": "infrastructure",
        "expected_context_candidate": "Infrastructure",
        "ambiguity": "Silk Road is infrastructure (trade routes) but also a historical concept and a website.",
        "disambiguation_test": "Should get 'infrastructure' or 'Infrastructure'.",
    },
    # --- 61-80: Biology, chemistry, and science ---
    {
        "id": "gold_element_vs_award_vs_color",
        "fact": "Gold has an atomic number of 79 and is one of the least reactive chemical elements, making it ideal for coins and jewelry.",
        "target_concept": "Gold",
        "expected_context_original": "chemical element",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Gold is a chemical element, a color, and an award (gold medal). The fact is about the element.",
        "disambiguation_test": "Should get 'chemical element' or 'Biomolecule' — the fact describes atomic properties.",
    },
    {
        "id": "mercury_element_vs_planet_vs_person",
        "fact": "Mercury is the closest planet to the Sun, with surface temperatures ranging from -173 to 427 degrees Celsius.",
        "target_concept": "Mercury",
        "expected_context_original": "planet",
        "expected_context_candidate": "Space",
        "ambiguity": "Mercury is a planet, a chemical element, and a Roman god. The fact is about the planet.",
        "disambiguation_test": "Should get 'planet' or 'Space' — the fact describes the planet.",
    },
    {
        "id": "oxygen_element_vs_molecule",
        "fact": "Oxygen constitutes approximately 21% of Earth's atmosphere and is essential for cellular respiration in most organisms.",
        "target_concept": "Oxygen",
        "expected_context_original": "chemical element",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Oxygen is a chemical element (O) and a molecule (O2). The fact discusses atmospheric composition.",
        "disambiguation_test": "Should get 'chemical element' or 'Biomolecule'.",
    },
    {
        "id": "dna_molecule_vs_gene",
        "fact": "DNA was identified as genetic material by Avery, MacLeod, and McCarty in 1944, before its double helix structure was solved by Watson and Crick.",
        "target_concept": "DNA",
        "expected_context_original": "Biomolecule",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "DNA is a biomolecule but also relates to genes and chemical compounds.",
        "disambiguation_test": "Should get 'Biomolecule' — DNA is a biological molecule.",
    },
    {
        "id": "insulin_hormone_vs_drug",
        "fact": "Insulin was discovered by Banting and Best in 1921, revolutionizing diabetes treatment and saving millions of lives.",
        "target_concept": "Insulin",
        "expected_context_original": "Hormone",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Insulin is a hormone but also a drug (medication).",
        "disambiguation_test": "Should get 'Hormone' or 'Biomolecule'.",
    },
    {
        "id": "penicillin_drug_vs_molecule",
        "fact": "Penicillin was discovered by Alexander Fleming in 1928 when he noticed that mold inhibited bacterial growth in a petri dish.",
        "target_concept": "Penicillin",
        "expected_context_original": "drug",
        "expected_context_candidate": "Disease",
        "ambiguity": "Penicillin is a drug but also a chemical compound/antibiotic.",
        "disambiguation_test": "Should get 'drug' or 'Disease' — it's about the medicine.",
    },
    {
        "id": "aspirin_drug_vs_brand",
        "fact": "Aspirin, synthesized by Felix Hoffmann at Bayer in 1897, remains one of the most widely used medications with over 100 million tablets consumed daily.",
        "target_concept": "Aspirin",
        "expected_context_original": "drug",
        "expected_context_candidate": "Disease",
        "ambiguity": "Aspirin is a drug but also a brand name and a chemical compound.",
        "disambiguation_test": "Should get 'drug' or 'Disease'.",
    },
    {
        "id": "covid_disease_vs_event",
        "fact": "COVID-19 is caused by the SARS-CoV-2 virus, which was first identified in Wuhan, China in December 2019.",
        "target_concept": "COVID-19",
        "expected_context_original": "disease",
        "expected_context_candidate": "Disease",
        "ambiguity": "COVID-19 is a disease but also an event (the pandemic).",
        "disambiguation_test": "Should get 'disease' or 'Disease'.",
    },
    {
        "id": "malaria_disease_vs_concept",
        "fact": "Malaria kills over 600,000 people annually, primarily children under 5 in sub-Saharan Africa.",
        "target_concept": "Malaria",
        "expected_context_original": "disease",
        "expected_context_candidate": "Disease",
        "ambiguity": "Malaria is a disease but also a scientific concept (the study of malaria).",
        "disambiguation_test": "Should get 'disease' or 'Disease'.",
    },
    {
        "id": "alzheimer_disease_vs_person",
        "fact": "Alzheimer's disease was first described by Alois Alzheimer in 1906, characterized by amyloid plaques and neurofibrillary tangles.",
        "target_concept": "Alzheimer",
        "expected_context_original": "scientist",
        "expected_context_candidate": "Scientist",
        "ambiguity": "Alzheimer is a scientist (Alois) and a disease (Alzheimer's). The fact describes the person's discovery.",
        "disambiguation_test": "Should get 'scientist' or 'Scientist' — the fact describes the discoverer, not the disease itself.",
    },
    {
        "id": "brain_organ_vs_journal",
        "fact": "The brain consumes approximately 20% of the body's oxygen despite comprising only 2% of body weight.",
        "target_concept": "Brain",
        "expected_context_original": "brain",
        "expected_context_candidate": "Anatomical structure",
        "ambiguity": "Brain is an anatomical structure but also an academic journal named 'Brain'. The fact is about the organ.",
        "disambiguation_test": "Should get 'brain' or 'anatomical structure' or 'Anatomical structure'.",
    },
    {
        "id": "vitamin_biomolecule_vs_food",
        "fact": "Vitamin D deficiency affects an estimated 1 billion people worldwide, causing rickets in children and osteomalacia in adults.",
        "target_concept": "Vitamin D",
        "expected_context_original": "Biomolecule",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Vitamin D is a biomolecule but also a nutrient/food component and a supplement.",
        "disambiguation_test": "Should get 'Biomolecule' — it's a biological molecule.",
    },
    {
        "id": "iron_element_vs_mineral_vs_nutrient",
        "fact": "Iron has been smelted from ore since 1200 BC, revolutionizing tool-making and warfare with its superior strength over bronze.",
        "target_concept": "Iron",
        "expected_context_original": "chemical element",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Iron is a chemical element, a mineral, and a nutrient. The fact is about metallurgy.",
        "disambiguation_test": "Should get 'chemical element' or 'Biomolecule' — the fact is about the metal.",
    },
    {
        "id": "aluminium_element_vs_material",
        "fact": "Aluminium is the most abundant metal in Earth's crust, constituting 8% by weight, but was not isolated until 1825 by Oersted.",
        "target_concept": "Aluminium",
        "expected_context_original": "chemical element",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Aluminium is a chemical element but also a material used in manufacturing.",
        "disambiguation_test": "Should get 'chemical element' or 'Biomolecule'.",
    },
    {
        "id": "carbon_element_vs_concept",
        "fact": "Carbon forms the backbone of organic chemistry, with four valence electrons enabling covalent bonds with up to four other atoms.",
        "target_concept": "Carbon",
        "expected_context_original": "chemical element",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Carbon is a chemical element but also a scientific concept (carbon cycle, carbon footprint).",
        "disambiguation_test": "Should get 'chemical element' or 'Biomolecule'.",
    },
    {
        "id": "uranium_element_vs_substance",
        "fact": "Uranium-235, making up 0.7% of natural uranium, is fissile and used as fuel in nuclear reactors worldwide.",
        "target_concept": "Uranium",
        "expected_context_original": "chemical element",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Uranium is a chemical element but also a chemical substance and a fuel.",
        "disambiguation_test": "Should get 'chemical element' or 'Biomolecule'.",
    },
    {
        "id": "ethanol_compound_vs_beverage",
        "fact": "Ethanol, with the chemical formula C2H5OH, has been used as an antiseptic since ancient times, killing bacteria by denaturing proteins.",
        "target_concept": "Ethanol",
        "expected_context_original": "chemical compound",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Ethanol is a chemical compound but also the active ingredient in alcoholic beverages.",
        "disambiguation_test": "Should get 'chemical compound' or 'Biomolecule' — the fact describes chemistry.",
    },
    {
        "id": "nicotine_compound_vs_drug",
        "fact": "Nicotine, an alkaloid found in tobacco plants, binds to nicotinic acetylcholine receptors in the brain, causing addiction.",
        "target_concept": "Nicotine",
        "expected_context_original": "chemical compound",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Nicotine is a chemical compound but also a drug/medication.",
        "disambiguation_test": "Should get 'chemical compound' or 'Biomolecule'.",
    },
    {
        "id": "caffeine_compound_vs_drug_vs_food",
        "fact": "Caffeine, a purine alkaloid with formula C8H10N4O2, blocks adenosine receptors in the brain, reducing fatigue perception.",
        "target_concept": "Caffeine",
        "expected_context_original": "chemical compound",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Caffeine is a chemical compound, a drug, and a food/beverage component.",
        "disambiguation_test": "Should get 'chemical compound' or 'Biomolecule'.",
    },
    {
        "id": "glucose_compound_vs_biomolecule",
        "fact": "Glucose, with the molecular formula C6H12O6, is the primary energy source for cells, metabolized via glycolysis to produce ATP.",
        "target_concept": "Glucose",
        "expected_context_original": "chemical compound",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Glucose is a chemical compound but also a biomolecule and a nutrient.",
        "disambiguation_test": "Should get 'chemical compound' or 'Biomolecule'.",
    },
    # --- 81-100: Media, culture, technology, and mixed ---
    {
        "id": "python_language_vs_animal",
        "fact": "Python was created by Guido van Rossum in 1991, designed with a readable syntax that emphasizes code clarity.",
        "target_concept": "Python",
        "expected_context_original": "programming language",
        "expected_context_candidate": "Software",
        "ambiguity": "Python is a programming language but also a snake (animal) and a Greek myth.",
        "disambiguation_test": "Should get 'programming language' or 'Software' — the fact is about the language.",
    },
    {
        "id": "java_language_vs_island_vs_coffee",
        "fact": "Java was released by Sun Microsystems in 1995 with the promise of 'write once, run anywhere' portability.",
        "target_concept": "Java",
        "expected_context_original": "programming language",
        "expected_context_candidate": "Software",
        "ambiguity": "Java is a programming language, an island, and coffee slang. The fact is about the language.",
        "disambiguation_test": "Should get 'programming language' or 'Software'.",
    },
    {
        "id": "linux_software_vs_person",
        "fact": "Linux was created by Linus Torvalds in 1991 as a free Unix-like operating system kernel.",
        "target_concept": "Linux",
        "expected_context_original": "software",
        "expected_context_candidate": "Software",
        "ambiguity": "Linux is software but also associated with the person Linus Torvalds.",
        "disambiguation_test": "Should get 'software' or 'Software'.",
    },
    {
        "id": "internet_software_vs_infrastructure",
        "fact": "The internet originated from ARPANET in 1969, with the first message sent between UCLA and Stanford on October 29.",
        "target_concept": "Internet",
        "expected_context_original": "software",
        "expected_context_candidate": "Software",
        "ambiguity": "Internet is both software/website and infrastructure.",
        "disambiguation_test": "Should get 'software' or 'Software' or 'infrastructure' or 'Infrastructure'.",
    },
    {
        "id": "facebook_company_vs_website",
        "fact": "Facebook was founded by Mark Zuckerberg in 2004 from his Harvard dorm room and reached 1 billion users by 2012.",
        "target_concept": "Facebook",
        "expected_context_original": "company",
        "expected_context_candidate": "Organisation",
        "ambiguity": "Facebook is a company but also a website and software.",
        "disambiguation_test": "Should get 'company' or 'Organisation' or 'website' or 'Software'.",
    },
    {
        "id": "google_company_vs_website_vs_concept",
        "fact": "Google was incorporated on September 4, 1998, by Larry Page and Sergey Brin with a $100,000 investment from Andy Bechtolsheim.",
        "target_concept": "Google",
        "expected_context_original": "company",
        "expected_context_candidate": "Organisation",
        "ambiguity": "Google is a company but also a website and a verb (to google).",
        "disambiguation_test": "Should get 'company' or 'Organisation' or 'website' or 'Software'.",
    },
    {
        "id": "batman_character_vs_place",
        "fact": "Batman first appeared in Detective Comics #27 in May 1939, created by Bob Kane and Bill Finger.",
        "target_concept": "Batman",
        "expected_context_original": "fictional character",
        "expected_context_candidate": "Fictional character",
        "ambiguity": "Batman is a fictional character but also a place (Batman, Turkey).",
        "disambiguation_test": "Should get 'fictional character' or 'Fictional character'.",
    },
    {
        "id": "superman_character_vs_person",
        "fact": "Superman was created by Jerry Siegel and Joe Shuster in 1933, debuting in Action Comics #1 in 1938.",
        "target_concept": "Superman",
        "expected_context_original": "fictional character",
        "expected_context_candidate": "Fictional character",
        "ambiguity": "Superman is a fictional character but could be a person (nickname).",
        "disambiguation_test": "Should get 'fictional character' or 'Fictional character'.",
    },
    {
        "id": "homer_character_vs_writer",
        "fact": "Homer is traditionally credited with composing the Iliad and the Odyssey, though his actual existence remains debated by scholars.",
        "target_concept": "Homer",
        "expected_context_original": "writer",
        "expected_context_candidate": "Writer",
        "ambiguity": "Homer is a writer (Greek poet) but also a fictional character (Homer Simpson).",
        "disambiguation_test": "Should get 'writer' or 'Writer'.",
    },
    {
        "id": "hamlet_character_vs_play_vs_person",
        "fact": "Hamlet, Prince of Denmark, is Shakespeare's longest play at 4,042 lines, written between 1599 and 1601.",
        "target_concept": "Hamlet",
        "expected_context_original": "book",
        "expected_context_candidate": "Book",
        "ambiguity": "Hamlet is a play (book), a fictional character, and a village (settlement). The fact describes the play.",
        "disambiguation_test": "Should get 'book' or 'Book' or 'play' or 'Theatre'.",
    },
    {
        "id": "jaws_film_vs_animal_vs_book",
        "fact": "Jaws, directed by Steven Spielberg in 1975, was the first film to gross over $100 million at the box office.",
        "target_concept": "Jaws",
        "expected_context_original": "movie",
        "expected_context_candidate": "Film",
        "ambiguity": "Jaws is a film, an animal body part, and a book. The fact is about the film.",
        "disambiguation_test": "Should get 'movie' or 'Film'.",
    },
    {
        "id": "avatar_film_vs_concept_vs_game",
        "fact": "Avatar, released in 2009, became the highest-grossing film of all time, earning $2.92 billion at the box office.",
        "target_concept": "Avatar",
        "expected_context_original": "movie",
        "expected_context_candidate": "Film",
        "ambiguity": "Avatar is a film, a concept (Hindu deity avatar), and a game.",
        "disambiguation_test": "Should get 'movie' or 'Film'.",
    },
    {
        "id": "star_wars_film_vs_event_vs_game",
        "fact": "Star Wars, released on May 25, 1977, by George Lucas, revolutionized science fiction cinema with its pioneering special effects.",
        "target_concept": "Star Wars",
        "expected_context_original": "movie",
        "expected_context_candidate": "Film",
        "ambiguity": "Star Wars is a film franchise, a cultural event, and a game series.",
        "disambiguation_test": "Should get 'movie' or 'Film'.",
    },
    {
        "id": "titanic_ship_vs_film_vs_event",
        "fact": "The Titanic sank on April 15, 1912, after striking an iceberg, resulting in over 1,500 deaths in one of history's worst peacetime maritime disasters.",
        "target_concept": "Titanic",
        "expected_context_original": "ship",
        "expected_context_candidate": "Transportation",
        "ambiguity": "Titanic is a ship, a film, and an event. The fact is about the ship sinking.",
        "disambiguation_test": "Should get 'ship' or 'Transportation' — the fact describes the vessel.",
    },
    {
        "id": "chess_game_vs_person_vs_concept",
        "fact": "Chess originated in India around the 6th century as Chaturanga, evolving into its modern form in 15th-century Europe.",
        "target_concept": "Chess",
        "expected_context_original": "game",
        "expected_context_candidate": "Game",
        "ambiguity": "Chess is a game but also a concept (chess strategy) and a music album.",
        "disambiguation_test": "Should get 'game' or 'Game'.",
    },
    {
        "id": "olympics_event_vs_place_vs_organisation",
        "fact": "The Olympics were held in Ancient Greece from 776 BC to 393 AD, held in Olympia every four years.",
        "target_concept": "Olympics",
        "expected_context_original": "olympics",
        "expected_context_candidate": "Sports competition",
        "ambiguity": "Olympics is an event, a place (Olympia), and an organisation (IOC).",
        "disambiguation_test": "Should get 'olympics' or 'Sports competition' or 'Event'.",
    },
    {
        "id": "nobel_prize_award_vs_person",
        "fact": "The Nobel Prize was established by Alfred Nobel's will in 1895, first awarded in 1901 for physics, chemistry, medicine, literature, and peace.",
        "target_concept": "Nobel Prize",
        "expected_context_original": "Nobel Prize",
        "expected_context_candidate": "Award",
        "ambiguity": "Nobel Prize is an award but also associated with a person (Alfred Nobel).",
        "disambiguation_test": "Should get 'Nobel Prize' or 'Award' or 'award'.",
    },
    {
        "id": "black_sabbath_band_vs_event_vs_concept",
        "fact": "Black Sabbath formed in Birmingham in 1968, with their self-titled debut album released on Friday the 13th, February 1970.",
        "target_concept": "Black Sabbath",
        "expected_context_original": "Band",
        "expected_context_candidate": "Musician",
        "ambiguity": "Black Sabbath is a band but also a religious concept and a day.",
        "disambiguation_test": "Should get 'Band' or 'Musician' or 'musical artist' or 'Artist'.",
    },
    {
        "id": "rush_band_vs_drug_vs_action",
        "fact": "Rush, the Canadian rock trio formed in 1968, released 19 studio albums and were inducted into the Rock and Roll Hall of Fame in 2013.",
        "target_concept": "Rush",
        "expected_context_original": "Band",
        "expected_context_candidate": "Musician",
        "ambiguity": "Rush is a band but also a drug (rush) and an action (to rush).",
        "disambiguation_test": "Should get 'Band' or 'Musician' or 'musical artist' or 'Artist'.",
    },
    {
        "id": "wikipedia_software_vs_organisation",
        "fact": "Wikipedia was launched on January 15, 2001, by Jimmy Wales and Larry Sanger, growing to over 55 million articles in 300 languages.",
        "target_concept": "Wikipedia",
        "expected_context_original": "website",
        "expected_context_candidate": "Software",
        "ambiguity": "Wikipedia is a website, software, and an organisation (Wikimedia Foundation).",
        "disambiguation_test": "Should get 'website' or 'Software' or 'organisation' or 'Organisation'.",
    },
    {
        "id": "fentanyl_drug_vs_compound",
        "fact": "Fentanyl is 50 to 100 times more potent than morphine, with a lethal dose as low as 2 milligrams in non-tolerant individuals.",
        "target_concept": "Fentanyl",
        "expected_context_original": "drug",
        "expected_context_candidate": "Disease",
        "ambiguity": "Fentanyl is a drug but also a chemical compound and a cause of death.",
        "disambiguation_test": "Should get 'drug' or 'Disease' or 'Medicine'.",
    },
    {
        "id": "ibuprofen_drug_vs_compound",
        "fact": "Ibuprofen was patented in 1961 by Stewart Adams at Boots UK, and works by inhibiting cyclooxygenase enzymes COX-1 and COX-2.",
        "target_concept": "Ibuprofen",
        "expected_context_original": "drug",
        "expected_context_candidate": "Disease",
        "ambiguity": "Ibuprofen is a drug but also a chemical compound.",
        "disambiguation_test": "Should get 'drug' or 'Disease' or 'Medicine'.",
    },
    {
        "id": "viagra_drug_vs_brand",
        "fact": "Viagra, containing the active ingredient sildenafil, was approved by the FDA on March 27, 1998, for treating erectile dysfunction.",
        "target_concept": "Viagra",
        "expected_context_original": "drug",
        "expected_context_candidate": "Disease",
        "ambiguity": "Viagra is a drug (brand name for sildenafil) but also a cultural phenomenon.",
        "disambiguation_test": "Should get 'drug' or 'Disease' or 'Medicine'.",
    },
    {
        "id": "stem_cell_biomolecule_vs_concept",
        "fact": "Stem cells were first identified in 1963 by McCulloch and Till, who found cells in mouse bone marrow that could regenerate blood tissue.",
        "target_concept": "Stem Cells",
        "expected_context_original": "Scientific concept",
        "expected_context_candidate": "Concept",
        "ambiguity": "Stem cells are biological entities but also a scientific concept and research field.",
        "disambiguation_test": "Should get 'Scientific concept' or 'Concept' or 'anatomical structure' or 'Anatomical structure'.",
    },
    {
        "id": "prayer_activity_vs_concept",
        "fact": "Christian prayer in the early church involved communal recitation of the Lord's Prayer three times daily, documented by Tertullian around 200 AD.",
        "target_concept": "Christian Prayer",
        "expected_context_original": "activity",
        "expected_context_candidate": "Activity",
        "ambiguity": "Christian Prayer is both an activity (the practice) and a Theological concept.",
        "disambiguation_test": "Should get 'activity' or 'Activity' — the fact describes the practice.",
    },
    {
        "id": "meditation_activity_vs_concept",
        "fact": "Meditation, particularly mindfulness-based interventions, reduces cortisol levels by approximately 20% according to a 2014 meta-analysis of 47 trials.",
        "target_concept": "Meditation",
        "expected_context_original": "activity",
        "expected_context_candidate": "Activity",
        "ambiguity": "Meditation is both an activity (the practice) and a scientific concept (mindfulness).",
        "disambiguation_test": "Should get 'activity' or 'Activity'.",
    },
    {
        "id": "compost_substance_vs_food_vs_concept",
        "fact": "Compost is produced by decomposing organic matter at temperatures between 49-77 degrees Celsius, requiring a carbon-to-nitrogen ratio of 30:1.",
        "target_concept": "Compost",
        "expected_context_original": "chemical substance",
        "expected_context_candidate": "Biomolecule",
        "ambiguity": "Compost is a chemical substance, but also classified as Food (compost is organic matter) and a topical concept.",
        "disambiguation_test": "Should get 'chemical substance' or 'Biomolecule' or 'topical concept' or 'Concept'.",
    },
    {
        "id": "gravity_concept_vs_person_vs_place",
        "fact": "Gravity, as described by Newton's law of universal gravitation, is proportional to the product of masses and inversely proportional to the square of the distance between them.",
        "target_concept": "Gravity",
        "expected_context_original": "Scientific concept",
        "expected_context_candidate": "Concept",
        "ambiguity": "Gravity is a scientific concept but also a film title and a place name (Gravity, Colorado).",
        "disambiguation_test": "Should get 'Scientific concept' or 'Concept'.",
    },
]


def build_prompt(fact: str, context_list: list[str]) -> str:
    rendered = "\n".join(f"- {c}" for c in context_list)
    return PROMPT_TEMPLATE % (rendered, fact)


def load_original_labels(path: Path) -> list[str]:
    with open(path) as f:
        labels = json.load(f)
    if not isinstance(labels, list):
        raise SystemExit(f"unexpected label file shape: {type(labels)}")
    return labels


def load_candidate_labels(path: Path) -> list[str]:
    with open(path) as f:
        data = json.load(f)
    if "categories" in data:
        return [c["label"] for c in data["categories"]]
    if isinstance(data, list):
        return data
    raise SystemExit(f"unexpected candidate file shape: {list(data.keys())}")


def chat_openrouter(prompt: str, model: str) -> str:
    api_key = os.environ.get("OPENROUTER_API_KEY", "")
    if not api_key:
        raise RuntimeError("OPENROUTER_API_KEY not set")
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": 0.0,
        "max_tokens": 2000,
    }).encode()
    req = urllib.request.Request(
        OPENROUTER_URL,
        data=body,
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=120) as resp:
        payload = json.loads(resp.read())
    choices = payload.get("choices", [])
    if not choices:
        return ""
    return choices[0].get("message", {}).get("content", "")


def parse_concepts(raw: str) -> list[dict]:
    raw = raw.strip()
    # strip markdown fences
    if raw.startswith("```"):
        raw = raw.split("\n", 1)[-1] if "\n" in raw else raw
        raw = raw.replace("```", "")
        raw = raw.strip()
    if not raw or raw == "[]":
        return []
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        # try extracting JSON array
        start = raw.find("[")
        end = raw.rfind("]")
        if start != -1 and end > start:
            try:
                return json.loads(raw[start : end + 1])
            except json.JSONDecodeError:
                pass
    return []


def find_concept(concepts: list[dict], target: str) -> dict | None:
    target_lower = target.lower()
    for c in concepts:
        if c.get("concept", "").lower() == target_lower:
            return c
    # fuzzy: target is a substring of concept or vice versa
    for c in concepts:
        con = c.get("concept", "").lower()
        if target_lower in con or con in target_lower:
            return c
    return None


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--original", type=Path, default=DEFAULT_ORIGINAL)
    ap.add_argument("--candidate", type=Path, default=DEFAULT_CANDIDATE)
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT)
    ap.add_argument("--resume", action="store_true",
                    help="resume from checkpoint if --out exists (skip completed cases)")
    args = ap.parse_args()

    original_labels = load_original_labels(args.original)
    candidate_labels = load_candidate_labels(args.candidate)

    print(f"original list: {len(original_labels)} labels ({args.original.name})")
    print(f"candidate list: {len(candidate_labels)} labels ({args.candidate.name})")
    print(f"benchmark cases: {len(BENCHMARK_CASES)}")
    print()

    # Resume support: load existing results and skip completed cases.
    results: list[dict] = []
    done_ids: set[str] = set()
    if args.resume and args.out.exists():
        try:
            old = json.load(open(args.out))
            results = old.get("cases", [])
            done_ids = {r["id"] for r in results}
            print(f"resuming: {len(results)} cases already done, skipping them")
        except (json.JSONDecodeError, KeyError):
            pass

    def checkpoint():
        """Write partial results so a timeout or crash doesn't lose progress."""
        of = sum(1 for r in results if r["original"]["found"])
        oc = sum(1 for r in results if r["original"]["correct"])
        cf = sum(1 for r in results if r["candidate"]["found"])
        cc = sum(1 for r in results if r["candidate"]["correct"])
        with open(args.out, "w") as f:
            json.dump({
                "model": CHAT_MODEL,
                "original_label_count": len(original_labels),
                "candidate_label_count": len(candidate_labels),
                "summary": {
                    "original_found": of,
                    "original_correct": oc,
                    "candidate_found": cf,
                    "candidate_correct": cc,
                    "total_cases": len(results),
                },
                "cases": results,
            }, f, indent=2)

    for i, case in enumerate(BENCHMARK_CASES):
        if case["id"] in done_ids:
            continue
        print(f"[{i+1}/{len(BENCHMARK_CASES)}] {case['id']} ...", end=" ", flush=True)

        prompt_orig = build_prompt(case["fact"], original_labels)
        prompt_cand = build_prompt(case["fact"], candidate_labels)

        try:
            raw_orig = chat_openrouter(prompt_orig, CHAT_MODEL)
        except Exception as e:
            raw_orig = ""
            print(f"ORIG-ERR({e}) ", end="", flush=True)
        try:
            raw_cand = chat_openrouter(prompt_cand, CHAT_MODEL)
        except Exception as e:
            raw_cand = ""
            print(f"CAND-ERR({e}) ", end="", flush=True)

        concepts_orig = parse_concepts(raw_orig)
        concepts_cand = parse_concepts(raw_cand)

        match_orig = find_concept(concepts_orig, case["target_concept"])
        match_cand = find_concept(concepts_cand, case["target_concept"])

        ctx_orig = match_orig.get("context", "") if match_orig else ""
        ctx_cand = match_cand.get("context", "") if match_cand else ""

        # Score
        orig_correct = ctx_orig.lower() == case["expected_context_original"].lower() if ctx_orig else False
        cand_correct = ctx_cand.lower() == case["expected_context_candidate"].lower() if ctx_cand else False
        orig_found = match_orig is not None
        cand_found = match_cand is not None

        result = {
            "id": case["id"],
            "fact": case["fact"],
            "target_concept": case["target_concept"],
            "expected_original": case["expected_context_original"],
            "expected_candidate": case["expected_context_candidate"],
            "ambiguity": case["ambiguity"],
            "original": {
                "all_concepts": [{"concept": c.get("concept", ""), "context": c.get("context", "")} for c in concepts_orig],
                "target_match": ctx_orig,
                "found": orig_found,
                "correct": orig_correct,
            },
            "candidate": {
                "all_concepts": [{"concept": c.get("concept", ""), "context": c.get("context", "")} for c in concepts_cand],
                "target_match": ctx_cand,
                "found": cand_found,
                "correct": cand_correct,
            },
        }
        results.append(result)

        status = "OK" if cand_correct else ("WRONG" if cand_found else "MISS")
        print(f"orig={ctx_orig!r}({'Y' if orig_correct else 'N'}) cand={ctx_cand!r}({'Y' if cand_correct else 'N'}) -> {status}")

        # Checkpoint after every case so a timeout doesn't lose progress.
        checkpoint()

    # Summary
    orig_found = sum(1 for r in results if r["original"]["found"])
    orig_correct = sum(1 for r in results if r["original"]["correct"])
    cand_found = sum(1 for r in results if r["candidate"]["found"])
    cand_correct = sum(1 for r in results if r["candidate"]["correct"])

    print()
    print("=" * 80)
    print(f"{'metric':<40} {'original (789)':>15} {'candidate ('+str(len(candidate_labels))+')':>15}")
    print("-" * 80)
    print(f"{'target concept found':<40} {orig_found:>10}/{len(results)} {cand_found:>10}/{len(results)}")
    print(f"{'correct context assigned':<40} {orig_correct:>10}/{len(results)} {cand_correct:>10}/{len(results)}")
    print(f"{'accuracy (of found)':<40} {orig_correct/max(orig_found,1)*100:>13.0f}% {cand_correct/max(cand_found,1)*100:>13.0f}%")
    print(f"{'accuracy (of total)':<40} {orig_correct/len(results)*100:>13.0f}% {cand_correct/len(results)*100:>13.0f}%")
    print("=" * 80)

    # Detailed failures
    print("\nFailures (candidate assigned wrong context or missed concept):")
    print("-" * 80)
    any_fail = False
    for r in results:
        if not r["candidate"]["correct"]:
            any_fail = True
            print(f"  {r['id']}: target={r['target_concept']!r}")
            print(f"    expected={r['expected_candidate']!r}, got={r['candidate']['target_match']!r}")
            print(f"    original got={r['original']['target_match']!r} (expected={r['expected_original']!r})")
    if not any_fail:
        print("  (none)")

    checkpoint()
    print(f"\nwrote {args.out}")


if __name__ == "__main__":
    main()
import type { Layer } from "@deck.gl/core";
import { ScatterplotLayer, TextLayer } from "@deck.gl/layers";
import { MapboxOverlay } from "@deck.gl/mapbox";
import maplibregl from "maplibre-gl";
import "maplibre-gl/dist/maplibre-gl.css";
import { useNavigate } from "react-router-dom";
import { useEffect, useMemo, useRef, useState } from "react";
import {
  clientColor,
  hexRGB,
  networkRGB,
  OTHER_COLOR,
  shortId,
} from "../theme";
import { pointAccuracyKM, pointSubdivision } from "../types";
import type { MapPoint } from "../types";

// Vector labels stay crisp at fractional zooms where raster label tiles blur.
const STYLE_URL =
  "https://basemaps.cartocdn.com/gl/dark-matter-gl-style/style.json";
const INITIAL = {
  center: [5, 25] as [number, number],
  zoom: 1.1,
  minZoom: 0.5,
  maxZoom: 12,
};
const LAND = "#161c28";
const WATER = "#070a11";

const NAME_TOKEN = /\{name/;

// MaxMind city-level fixes are ≤100 km; 200+ means a region/country centroid — a synthetic point, not a location.
const IMPRECISE_KM = 200;
const STACK_MIN = 10;
const BADGE_RGB = hexRGB(OTHER_COLOR);
const POPUP_LIST_MAX = 30;

function restyleBasemap(map: maplibregl.Map) {
  for (const layer of map.getStyle().layers) {
    if (layer.type === "background") {
      map.setPaintProperty(layer.id, "background-color", LAND);
    } else if (layer.type === "fill" && layer["source-layer"] === "water") {
      if (layer.id !== "water_shadow")
        map.setPaintProperty(layer.id, "fill-color", WATER);
    } else if (
      layer.type === "fill" &&
      ["landcover", "landuse", "park"].includes(layer["source-layer"] ?? "")
    ) {
      // near-black fills in the stock style; they read as stains on the recolored land
      map.setLayoutProperty(layer.id, "visibility", "none");
    } else if (layer.type === "symbol") {
      const field = map.getLayoutProperty(layer.id, "text-field") as
        string | { stops?: [number, string][] } | undefined;
      const usesName =
        typeof field === "string"
          ? NAME_TOKEN.test(field)
          : Boolean(
              field?.stops?.some(
                ([, value]) =>
                  typeof value === "string" && NAME_TOKEN.test(value),
              ),
            );
      // The stock style falls back to local names at some zooms; force English.
      if (usesName)
        map.setLayoutProperty(layer.id, "text-field", [
          "coalesce",
          ["get", "name_en"],
          ["get", "name"],
        ]);
    }
  }
}

interface MapCluster {
  key: string;
  longitude: number;
  latitude: number;
  points: MapPoint[];
  el: number;
  cl: number;
  colorPoint: MapPoint;
  innerPoint: MapPoint | null;
  accuracyKM: number;
}

// accuracyKM is the cluster min: a lone coarse record can share coordinates with a real city (Ashburn).
const stacked = (cluster: MapCluster) =>
  cluster.accuracyKM >= IMPRECISE_KM && cluster.points.length >= STACK_MIN;

function placeFor(point: MapPoint): string {
  return (
    [point[5], pointSubdivision(point), point[4]].filter(Boolean).join(", ") ||
    "Unknown location"
  );
}

function layerName(point: MapPoint): string {
  return point[6] === "el"
    ? "Execution"
    : point[6] === "cl"
      ? "Consensus"
      : "Unknown layer";
}

export default function WorldMap({
  points,
  network,
  colorFor,
  colorKey,
  legend,
}: {
  points: MapPoint[] | null;
  network: string;
  colorFor?: (p: MapPoint) => [number, number, number, number];
  colorKey?: string;
  legend?: { name: string; color: string }[];
}) {
  const navigate = useNavigate();
  const containerRef = useRef<HTMLDivElement | null>(null);
  const mapRef = useRef<maplibregl.Map | null>(null);
  const overlayRef = useRef<MapboxOverlay | null>(null);
  const [selectedKey, setSelectedKey] = useState<string | null>(null);
  const baseColor = useMemo<[number, number, number, number]>(
    () => [...networkRGB(network), 220],
    [network],
  );
  const clusters = useMemo(() => {
    const grouped = new Map<string, MapPoint[]>();
    for (const point of points ?? []) {
      const key = `${point[1]},${point[2]}`;
      const members = grouped.get(key);
      if (members) members.push(point);
      else grouped.set(key, [point]);
    }
    return [...grouped.entries()].map(([key, members]): MapCluster => {
      const clients = new Map<string, number>();
      let el = 0;
      let cl = 0;
      for (const point of members) {
        const client = point[3] || "unknown";
        clients.set(client, (clients.get(client) ?? 0) + 1);
        if (point[6] === "el") el++;
        if (point[6] === "cl") cl++;
      }
      const ranked = [...clients.entries()].sort(
        (a, b) => b[1] - a[1] || a[0].localeCompare(b[0]),
      );
      const [dominantClient] = ranked[0];
      const known = ranked
        .map(([name]) => name)
        .filter((name) => name !== "unknown");
      const innerClient =
        known.length >= 2
          ? known.find((name) => name !== dominantClient)
          : undefined;
      const first = members[0];
      const pointWith = (client: string): MapPoint => [
        first[0],
        first[1],
        first[2],
        client,
        first[4],
        first[5],
        first[6],
        first[7],
        first[8],
        first[9],
        first[10],
        first[11],
      ];
      return {
        key,
        longitude: first[1],
        latitude: first[2],
        points: members,
        el,
        cl,
        colorPoint: pointWith(
          dominantClient === "unknown" ? "" : dominantClient,
        ),
        innerPoint: innerClient ? pointWith(innerClient) : null,
        accuracyKM: Math.min(...members.map(pointAccuracyKM)),
      };
    });
  }, [points]);
  const selected = selectedKey
    ? (clusters.find((cluster) => cluster.key === selectedKey) ?? null)
    : null;

  useEffect(() => setSelectedKey(null), [network]);
  useEffect(() => {
    // Keep the popup open across background refetches, but drop a selection whose
    // cluster disappeared so it cannot resurrect if the same key reappears later.
    if (selectedKey && !clusters.some((cluster) => cluster.key === selectedKey)) {
      setSelectedKey(null);
    }
  }, [clusters, selectedKey]);

  useEffect(() => {
    const map = new maplibregl.Map({
      container: containerRef.current!,
      style: STYLE_URL,
      center: INITIAL.center,
      zoom: INITIAL.zoom,
      minZoom: INITIAL.minZoom,
      maxZoom: INITIAL.maxZoom,
      attributionControl: false,
      dragRotate: false,
      pitchWithRotate: false,
      touchPitch: false,
    });
    map.touchZoomRotate.disableRotation();
    map.keyboard.disableRotation();
    map.on("style.load", () => restyleBasemap(map));
    const overlay = new MapboxOverlay({ interleaved: false });
    map.addControl(overlay);
    mapRef.current = map;
    overlayRef.current = overlay;
    return () => {
      mapRef.current = null;
      overlayRef.current = null;
      map.remove();
    };
  }, []);

  useEffect(() => {
    const located = clusters.filter((cluster) => !stacked(cluster));
    const badges = clusters.filter(stacked);
    const badgeRadius = (cluster: MapCluster) =>
      Math.min(16, 8 + Math.sqrt(cluster.points.length) / 2.5);
    const layers: Layer[] = [
      new ScatterplotLayer<MapCluster>({
        id: "badges",
        data: badges,
        getPosition: (cluster) => [cluster.longitude, cluster.latitude],
        getRadius: badgeRadius,
        radiusUnits: "pixels",
        getFillColor: [...BADGE_RGB, 45],
        getLineColor: (cluster) =>
          cluster.key === selectedKey
            ? [255, 255, 255, 245]
            : [...BADGE_RGB, 200],
        getLineWidth: (cluster) => (cluster.key === selectedKey ? 2 : 1.2),
        updateTriggers: {
          getLineColor: selectedKey,
          getLineWidth: selectedKey,
        },
        pickable: true,
        stroked: true,
        lineWidthUnits: "pixels",
        autoHighlight: true,
        highlightColor: [255, 255, 255, 40],
      }),
      new ScatterplotLayer<MapCluster>({
        id: "nodes",
        data: located,
        getPosition: (cluster) => [cluster.longitude, cluster.latitude],
        getFillColor: colorFor
          ? (cluster) => colorFor(cluster.colorPoint)
          : () => baseColor,
        getLineColor: (cluster) =>
          cluster.key === selectedKey ? [255, 255, 255, 245] : [8, 11, 18, 210],
        getLineWidth: (cluster) => (cluster.key === selectedKey ? 2.5 : 1),
        updateTriggers: {
          getFillColor: colorKey ?? network,
          getLineColor: selectedKey,
          getLineWidth: selectedKey,
        },
        getRadius: (cluster) => (cluster.points.length > 1 ? 5.5 : 4),
        radiusUnits: "pixels",
        pickable: true,
        stroked: true,
        lineWidthUnits: "pixels",
        autoHighlight: true,
        highlightColor: [255, 255, 255, 55],
      }),
    ];
    if (colorFor) {
      layers.push(
        new ScatterplotLayer<MapCluster>({
          id: "nodes-core",
          data: located.filter((cluster) => cluster.innerPoint),
          getPosition: (cluster) => [cluster.longitude, cluster.latitude],
          getFillColor: (cluster) => colorFor(cluster.innerPoint!),
          updateTriggers: { getFillColor: colorKey ?? network },
          getRadius: 2.6,
          radiusUnits: "pixels",
          stroked: false,
          pickable: false,
        }),
      );
    }
    layers.push(
      new TextLayer<MapCluster>({
        id: "badge-counts",
        data: badges,
        getPosition: (cluster) => [cluster.longitude, cluster.latitude],
        getText: (cluster) => String(cluster.points.length),
        getSize: 10,
        getColor: [220, 227, 238, 235],
        fontFamily: "system-ui, sans-serif",
        fontWeight: 600,
        pickable: false,
      }),
    );
    overlayRef.current?.setProps({
      layers,
      onClick: (info) => {
        const cluster = info.object as MapCluster | undefined;
        setSelectedKey(cluster ? cluster.key : null);
      },
      onHover: (info) => {
        const canvas = mapRef.current?.getCanvas();
        if (canvas) canvas.style.cursor = info.object ? "pointer" : "";
      },
      getTooltip: ({ object }) => {
        const cluster = object as MapCluster | null;
        if (!cluster) return null;
        const where = stacked(cluster)
          ? `somewhere in ${cluster.points[0][4] || "an unknown country"} (location only accurate to ±${cluster.accuracyKM.toLocaleString()} km)`
          : placeFor(cluster.points[0]);
        return {
          // Use deck.gl `text` (innerText), never `html` (innerHTML), for peer-controlled place names.
          text: `${cluster.points.length} ${cluster.points.length === 1 ? "identity" : "identities"} · ${where}\nClick for details`,
          style: {
            background: "#11161f",
            color: "#e6ebf3",
            fontSize: "12px",
            borderRadius: "6px",
            padding: "6px 9px",
            lineHeight: "1.5",
          },
        };
      },
    });
  }, [baseColor, clusters, colorFor, colorKey, navigate, network, selectedKey]);

  return (
    <div className="map">
      <div ref={containerRef} className="map-canvas" />
      {legend && legend.length > 0 && (
        <div className="map-legend">
          {legend.map((entry) => (
            <span key={entry.name}>
              <span
                className="legend-swatch"
                style={{ background: entry.color }}
              />
              {entry.name}
            </span>
          ))}
          {clusters.some(stacked) && (
            <span>
              <span className="legend-swatch legend-swatch-halo" />
              country-level only
            </span>
          )}
        </div>
      )}
      {selected && (
        <aside className="map-popup" aria-label="Selected map identities">
          <button
            className="map-popup-close"
            aria-label="Close map details"
            onClick={() => setSelectedKey(null)}
          >
            ×
          </button>
          <p className="map-popup-kicker">
            {selected.points.length === 1
              ? "IDENTITY"
              : `${selected.points.length} IDENTITIES`}
          </p>
          <h3>
            {stacked(selected)
              ? `Somewhere in ${selected.points[0][4] || "an unknown country"}`
              : placeFor(selected.points[0])}
          </h3>
          <p className="map-popup-summary">
            {network} · EL {selected.el} · CL {selected.cl}
          </p>
          {stacked(selected) && (
            <p className="map-popup-note">
              GeoIP can place these identities only within ±
              {selected.accuracyKM.toLocaleString()} km, so they share one
              approximate point.
            </p>
          )}
          {selected.points.length > 1 && !stacked(selected) && (
            <button
              className="map-popup-zoom"
              onClick={() => {
                const map = mapRef.current;
                if (!map) return;
                map.easeTo({
                  center: [selected.longitude, selected.latitude],
                  zoom: Math.min(map.getZoom() + 2, INITIAL.maxZoom),
                });
              }}
            >
              Zoom in here
            </button>
          )}
          <div className="map-popup-list">
            {selected.points.slice(0, POPUP_LIST_MAX).map((point) => (
              <button
                className="map-popup-node"
                key={point[0]}
                onClick={() => navigate(`/nodes/${point[0]}`)}
              >
                <span
                  className="legend-swatch"
                  style={{ background: clientColor(point[3]) }}
                />
                <span>
                  <strong>{point[3] || "Unknown client"}</strong>
                  <small>
                    {layerName(point)} · {shortId(point[0], 14)}
                  </small>
                </span>
                <span className="map-popup-arrow">→</span>
              </button>
            ))}
            {selected.points.length > POPUP_LIST_MAX && (
              <p className="map-popup-more">
                …and{" "}
                {(selected.points.length - POPUP_LIST_MAX).toLocaleString()}{" "}
                more identities here
              </p>
            )}
          </div>
        </aside>
      )}
      <div className="map-attr">
        <a
          href="https://www.openstreetmap.org/copyright"
          target="_blank"
          rel="noreferrer"
        >
          © OpenStreetMap contributors
        </a>
        {" · "}
        <a
          href="https://carto.com/attributions"
          target="_blank"
          rel="noreferrer"
        >
          © CARTO
        </a>
        {" · GeoLite2 data © MaxMind"}
      </div>
    </div>
  );
}

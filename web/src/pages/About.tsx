import { useEffect, useState } from "react";
import { useLocation } from "react-router";
import { fetchMeta } from "../api";

const DEFAULT_SOURCE_URL = "https://github.com/MysticRyuujin/enrscout";

export default function About() {
  const [sourceURL, setSourceURL] = useState(DEFAULT_SOURCE_URL);
  const { hash } = useLocation();

  useEffect(() => {
    let live = true;
    fetchMeta()
      .then((meta) => {
        if (live && meta.source_url) setSourceURL(meta.source_url);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);

  useEffect(() => {
    if (hash) document.getElementById(hash.slice(1))?.scrollIntoView();
  }, [hash]);

  return (
    <div className="page about">
      <div className="page-head">
        <h1>About ENRScout</h1>
      </div>
      <div className="card prose">
        <p>
          ENRScout continuously crawls the Ethereum peer-to-peer discovery
          network over both <b>discv5</b> and <b>discv4</b>, across IPv4 and
          IPv6, and builds a live picture of nodes on <b>Mainnet</b>,{" "}
          <b>Hoodi</b>, and <b>Sepolia</b>.
        </p>

        <div className="note">
          <b>This is not a full census of the network.</b> ENRScout only shows
          nodes our crawler has discovered, resolved, and classified. Nodes
          behind restrictive NATs or firewalls, nodes that never advertise
          themselves over discovery, and nodes we simply haven't encountered yet
          are missing. Treat the counts and breakdowns as an observed,
          non-random identity sample, not the complete set of participants on
          each network. Coverage gaps can reduce the sample, while Sybil ENRs,
          key rotation, and the lack of per-IP or per-ASN caps can increase
          identity counts relative to machines or operators. A typical Ethereum
          full node runs one execution identity and one consensus identity, so
          adding both layers can already produce roughly twice as many
          identities as machines. ENRScout observes from a single crawler
          vantage point, and none of its figures are a census, machine or
          operator count, validator count, or stake-weighted client-diversity
          estimate. The filter works in the other direction too: an identity is
          published only after the crawler directly resolves its record or
          authenticates it over a real connection, so DHT-served leads that
          never respond are held back rather than counted.
        </div>

        <h3>How classification works</h3>
        <p>
          The discv5 and discv4 DHTs are a single shared keyspace, so a node
          from any chain can turn up in the crawl. We assign each node to a
          network rather than filtering the crawl: <b>execution</b> nodes are
          bucketed by their EIP-2124 fork id, and <b>consensus</b> nodes by
          their fork digest. When an execution node is fingerprinted, the
          crawler also completes the authenticated <code>eth</code>{" "}
          <b>Status</b> exchange: the peer makes a live membership claim
          containing its genesis and network ID, and its live fork id replaces
          whatever the discovery record claimed. Execution peers on an old fork
          remain available through the API for audits, but are excluded from the
          website's totals, lists, maps, and DNS trees. Nodes that don't match
          one of the three tracked networks are excluded.
        </p>

        <h3 id="membership">
          Membership evidence: Status-verified vs ENR-claimed
        </h3>
        <p>
          Every identity records <i>how</i> its network membership was
          established. <b>Status-verified</b> means the peer made a live
          membership claim over an authenticated handshake - the RLPx{" "}
          <code>eth</code> Status exchange for execution nodes, or the consensus
          Status exchange for beacon nodes - so its network ID, genesis, or fork
          digest came from the holder of that node key during a real connection.{" "}
          <b>ENR-claimed</b> means the network is asserted only by the node's
          self-signed discovery record and has not yet been confirmed over a
          connection. Authentication binds the claim to the node key, but cannot
          prove a malicious or experimental peer honestly follows the claimed
          chain. The overview's membership toggle filters map points only;
          counts and charts continue to describe the full selected-network
          snapshot.
        </p>

        <h3>Enrichment</h3>
        <p>
          Client, version and OS are gathered by connecting to each node
          directly: an RLPx Hello plus the authenticated <code>eth</code> Status
          exchange for execution identities, and a libp2p identify probe for
          consensus nodes. Fingerprints arrive over two paths - outbound probes
          to discovered nodes, and inbound connections from peers that dial the
          crawler's own advertised per-network identities. The inbound path
          matters: many nodes are at their peer limit or rate-limit unknown
          dialers, so the connection they initiate is often the only way to
          identify them. Client-distribution charts use identified identities as
          their denominator; an identity whose client is still unknown counts
          toward network totals but not toward client percentages. A node's{" "}
          <b>discovery history</b> is separate from its fingerprint: it
          summarizes repeated successful direct resolutions, while failed
          resolutions reduce the score. It describes how consistently the
          crawler can resolve the node, not the accuracy of its identified
          client. Geolocation (country, subdivision, city, coordinates) and
          network operator (ASN and organization) come from MaxMind GeoLite2.
          Geolocation is approximate: coordinates are database locations or city
          centroids, and VPNs, hosting networks, or stale registrations can
          attribute an identity to the wrong place. Data is a rolling snapshot.
          The explorer exposes only the latest committed generation, and every
          figure reflects only the most recent crawl; operators may retain
          bounded older generations for rollback.
        </p>
        <p>
          Client names and versions remain self-reported: the successful
          handshake ties a claim to the peer's cryptographic identity, but does
          not independently prove which software produced it. The crawler's RLPx
          Hello deliberately advertises <code>eth/66..72 + snap</code> for
          interoperability. With the default three networks and one EL identity
          per network, its DHT footprint is six persistent advertiser identities
          (one EL and one CL per network).
        </p>

        <h3>Residential vs. cloud hosting</h3>
        <p>
          Each node's IP is mapped to its autonomous system (ASN) and operator
          name via GeoLite2. We label a node as <b>cloud / datacenter</b> when
          the operator name matches a known hosting or cloud provider (for
          example AWS, Google, Azure, Hetzner, OVH, DigitalOcean, Contabo, and
          similar), and <b>residential / other</b> otherwise. This is a
          heuristic based on the operator's name, not a guarantee: a residential
          ISP that also sells hosting, or a provider we don't yet recognize, can
          be mislabeled. Reachability (<b>dialable</b> vs. <b>discovery-only</b>
          ) is tracked separately and reflects whether the node advertises a
          usable TCP or QUIC port.
        </p>

        <h3 id="privacy">Data & privacy</h3>
        <p>
          ENRScout publishes information that Ethereum nodes broadcast on the
          public discovery network: node ID, IP address and ports, the signed
          ENR, observation timestamps, self-reported client/version/OS,
          membership evidence, and MaxMind GeoLite2 geolocation and ASN/operator
          for the node's IP. This is observed public network data — ENRScout has
          no accounts or login and collects no first-party analytics on
          visitors.
        </p>
        <p>
          When you view the map, your browser requests basemap tiles directly
          from CARTO, which necessarily exposes ordinary request metadata (such
          as your IP address and user agent) to CARTO under their terms.
          Questions or corrections about published node data can be sent to{" "}
          <a href="mailto:mysticryuujin@protonmail.com">
            mysticryuujin@protonmail.com
          </a>
          .
        </p>

        <h3>Data sources & credits</h3>
        <ul>
          <li>Discovery built on go-ethereum p2p libraries.</li>
          <li>
            GeoLite2 data © MaxMind -{" "}
            <a href="https://www.maxmind.com">https://www.maxmind.com</a>
          </li>
          <li>Basemap tiles © CARTO, map data © OpenStreetMap contributors.</li>
        </ul>

        <h3>Source and license</h3>
        <p>
          Copyright © 2026 the ENRScout authors. ENRScout is free software
          licensed under the GNU Affero General Public License, version 3 or
          later, and comes with no warranty. You may view, run, modify, and
          redistribute its source under that license. The corresponding source
          and license text are available in the{" "}
          <a href={sourceURL}>source corresponding to this deployment</a>.
        </p>
      </div>
    </div>
  );
}

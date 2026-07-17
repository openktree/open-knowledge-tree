import { Show } from "solid-js";
import Card from "../../components/Card";
import SourceHeader from "./SourceHeader";
import SourceBody from "./SourceBody";
import SourceImages from "./SourceImages";

/**
 * Composed view for a single source row. Receives the
 * already-fetched data and renders header + body + image
 * gallery. Empty / error states are handled by the
 * page-level index so this component stays presentation.
 *
 * Props:
 *   - source: accessor returning the source row
 *   - images: accessor returning the image list
 *   - slug:   string
 *   - sourceID: string (the route's :sourceID)
 *   - error:  string | null, the error column from the
 *     row (surfaced in the header)
 */
export default function SourceDetailContent(props) {
  return (
    <div class="space-y-6">
      <SourceHeader
        source={props.source}
        slug={props.slug}
        error={props.error}
      />

      <Card>
        <SourceBody
          source={props.source}
          slug={props.slug}
          sourceID={props.sourceID}
          highlightIndices={props.highlightIndices}
          factCounts={props.factCounts}
          onSentenceClick={props.onSentenceClick}
        />
      </Card>

      <Show when={(props.images() || []).length > 0}>
        <Card>
          <SourceImages
            images={props.images}
            slug={props.slug}
            sourceID={props.sourceID}
          />
        </Card>
      </Show>
    </div>
  );
}

import { CommandResult, ObjectRef } from "../models";
import { ObjectType } from "../api";
import React, { useContext } from "react";
import { CodeViewer } from "./CodeViewer";

import { Loading, useLoadingHelper } from "./Loading";
import { ApiContext } from "./App";
import * as yaml from 'js-yaml';
import { ErrorMessage } from "./ErrorMessage";
import { Box } from "@mui/material";

export const ObjectYaml = (props: { cr: CommandResult, objectRef: ObjectRef, objectType: ObjectType }) => {
    const api = useContext(ApiContext)
    const [loading, error, content] = useLoadingHelper<string>(async () => {
        const o = await api.getCommandResultObject(props.cr.id, props.objectRef, props.objectType)
        return yaml.dump(o)
    }, [props.cr.id, props.objectRef, props.objectType])

    if (loading) {
        return <Box width={"100%"}>
            <Loading />
        </Box>
    } else if (error) {
        return <ErrorMessage>
            {error.message}
        </ErrorMessage>
    } else {
        return <CodeViewer code={content!} language={"yaml"} />
    }
}
